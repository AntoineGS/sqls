package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

var (
	ErrNoConnection       = errors.New("no database connection")
	errConnectionChanging = errors.New("database connection is changing; retry shortly")
	errNoReadyConnection  = errors.New("database connection is not ready; retry shortly")
	errConnectionNotReady = errors.New("database connection is not ready; retry shortly")
)

type Server struct {
	SpecificFileCfg *config.Config
	DefaultFileCfg  *config.Config
	WSCfg           *config.Config

	// connMu guards connection lifetime. Commands that touch the database take
	// it for reading; commands that replace the connection take it for
	// writing. Lock ordering: connMu before stateMu, never the reverse.
	connMu sync.RWMutex

	// stateMu guards every mutable field below. It is taken for short,
	// non-blocking accesses only: connMu, not stateMu, is what a command holds
	// across database I/O.
	stateMu sync.RWMutex

	// diagnosticsPublishMu serializes version/generation validation with LSP
	// diagnostic sends. Connection generation changes use the same fence so a
	// publication from the previous attachment cannot follow the switch's
	// clearing/recompute notification.
	diagnosticsPublishMu sync.Mutex
	diagnosticCatalogMu  sync.Mutex
	diagnosticCache      *database.DBCache
	derivedCatalog       sqlsymbol.Catalog
	diagnosticWorkMu     sync.Mutex
	diagnosticDocuments  map[string]struct{}
	diagnosticAllOpen    bool
	diagnosticAnalyzer   func(documentDiagnosticsSnapshot) []lsp.Diagnostic

	dbConn *database.DBConnection

	curDBCfg           *database.DBConfig
	curDBName          string
	curConnectionIndex int

	// The initOptionDBConfig is an optional param
	// sent by the client as part of the LSP InitializationOptions
	// payload. If non-nil, the server will ignore all
	// other configuration sources (workspace and user).
	initOptionDBConfig *database.DBConfig

	// connGeneration advances on every reconnect. It ties per-connection
	// artefacts — the hover DDL memo, and the go-to-definition snapshot
	// directory — to the connection they were produced under. Guarded by
	// stateMu.
	connGeneration   int
	lifecycleCtx     context.Context
	lifecycleCancel  context.CancelFunc
	coordinator      *connectionCoordinator
	metadata         *database.MetadataLoader
	initialized      bool
	connectionState  connectionState
	metadataStartErr error
	openConnection   database.ContextOpener
	activeConfigKey  string
	cleanupOnce      sync.Once
	cleanupDone      chan struct{}
	cleanupQueue     chan *database.DBConnection
	cleanupFinal     chan *database.DBConnection
	diagnosticsWake  chan struct{}
	stopOnce         sync.Once
	fileRevision     uint64
	notificationConn *jsonrpc2.Conn

	// ddlMemo caches the rendered DDL appendix per connection generation.
	// Hover fires on every cursor rest over the same token; without this,
	// each one is a catalog round trip. Guarded by stateMu, and never held
	// across the round trip itself.
	ddlMemo map[ddlKey]string

	// snapshots materialises database-resident source as read-only files. It is
	// nil when the user cache directory could not be located, which disables
	// go-to-definition for database objects and nothing else.
	snapshots *sourceSnapshotStore

	files   map[string]*File
	cancels *cancelRegistry
}

type File struct {
	LanguageID string
	Text       string
	Version    int
	Revision   uint64
}

func NewServer() *Server {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())

	server := &Server{
		files:               make(map[string]*File),
		ddlMemo:             make(map[ddlKey]string),
		cancels:             newCancelRegistry(),
		lifecycleCtx:        lifecycleCtx,
		lifecycleCancel:     lifecycleCancel,
		metadata:            database.NewMetadataLoader(),
		connectionState:     connectionIdle,
		openConnection:      database.OpenContext,
		cleanupDone:         make(chan struct{}),
		cleanupQueue:        make(chan *database.DBConnection, 2),
		cleanupFinal:        make(chan *database.DBConnection, 1),
		diagnosticsWake:     make(chan struct{}, 1),
		diagnosticDocuments: make(map[string]struct{}),
	}
	server.metadata.SetChangedCallback(func() {
		server.queueAllDiagnostics()
	})
	server.coordinator = newConnectionCoordinator(server)
	go server.cleanupConnections()
	go server.runDiagnosticSignals()
	// Deliberately no filesystem access here: NewServer runs in every test in
	// this package, and touching the real cache directory from a unit test is
	// the hazard the injected root exists to remove. The root is only resolved,
	// never created or read, until the first snapshot is written.
	root, err := defaultSnapshotRoot()
	if err != nil {
		log.Printf("sqls: go-to-definition snapshots are disabled: %v", err)
	} else {
		server.snapshots = newSourceSnapshotStore(root)
	}
	return server
}

func panicf(r interface{}, format string, v ...interface{}) error {
	if r != nil {
		// Same as net/http
		const size = 64 << 10
		buf := make([]byte, size)
		buf = buf[:runtime.Stack(buf, false)]
		id := fmt.Sprintf(format, v...)
		log.Printf("panic serving %s: %v\n%s", id, r, string(buf))
		return fmt.Errorf("unexpected panic: %v", r)
	}
	return nil
}

// Stop closes the database connection, always stops the worker, and always
// removes this process's snapshots — including when closing the connection
// fails. A half-dead InterBase attachment is exactly the shutdown that fails,
// and it must not be the one that leaves database source on disk.
//
// It deliberately takes no connMu — a runaway query must not be able to hold
// the process open — but it does take stateMu for the pointer read, because a
// concurrent switch may be reassigning it.
func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		s.lifecycleCancel()
		s.coordinator.Stop()
		s.metadata.Stop()
		s.snapshots.BeginShutdown()
		s.stateMu.Lock()
		s.connectionState = connectionStopped
		dbConn := s.dbConn
		s.dbConn = nil
		s.stateMu.Unlock()
		s.cleanupFinal <- dbConn
		go func() { <-s.coordinator.done; close(s.cleanupQueue) }()
	})
	return nil
}

func (s *Server) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if conn != nil && !isServerNotification(req.Method) {
		s.stateMu.Lock()
		s.notificationConn = conn
		s.stateMu.Unlock()
	}
	// Prevent any uncaught panics from taking the entire server down.
	defer func() {
		if perr := panicf(recover(), "%v", req.Method); perr != nil {
			err = perr
		}
	}()
	res, err := s.handle(ctx, conn, req)
	if err != nil {
		log.Printf("error serving, %+v\n", err)
	}
	return res, err
}
func (s *Server) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, conn, req)
	case "initialized":
		s.handleInitialized()
		return
	case "shutdown":
		return s.handleShutdown(ctx, conn, req)
	case "exit":
		return s.handleExit(ctx, conn, req)
	case "textDocument/didOpen":
		return s.handleTextDocumentDidOpen(ctx, conn, req)
	case "textDocument/didChange":
		return s.handleTextDocumentDidChange(ctx, conn, req)
	case "textDocument/didSave":
		return s.handleTextDocumentDidSave(ctx, conn, req)
	case "textDocument/didClose":
		return s.handleTextDocumentDidClose(ctx, conn, req)
	case "textDocument/completion":
		return s.handleTextDocumentCompletion(ctx, conn, req)
	case "textDocument/hover":
		return s.handleTextDocumentHover(ctx, conn, req)
	case "textDocument/codeAction":
		return s.handleTextDocumentCodeAction(ctx, conn, req)
	case "workspace/executeCommand":
		return s.handleWorkspaceExecuteCommand(ctx, conn, req)
	case "workspace/didChangeConfiguration":
		return s.handleWorkspaceDidChangeConfiguration(ctx, conn, req)
	case "$/cancelRequest":
		return s.handleCancelRequest(ctx, conn, req)
	case "textDocument/formatting":
		return s.handleTextDocumentFormatting(ctx, conn, req)
	case "textDocument/rangeFormatting":
		return s.handleTextDocumentRangeFormatting(ctx, conn, req)
	case "textDocument/signatureHelp":
		return s.handleTextDocumentSignatureHelp(ctx, conn, req)
	case "textDocument/rename":
		return s.handleTextDocumentRename(ctx, conn, req)
	case "textDocument/definition":
		return s.handleDefinition(ctx, conn, req)
	case "textDocument/references":
		return s.handleReferences(ctx, conn, req)
	case "textDocument/typeDefinition":
		return s.handleDefinition(ctx, conn, req)
	case "window/showMessage":
		return
	}
	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
}

func (s *Server) handleInitialize(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.InitializeParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	result = lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			TextDocumentSync:   lsp.TDSKFull,
			HoverProvider:      true,
			CodeActionProvider: true,
			CompletionProvider: &lsp.CompletionOptions{
				TriggerCharacters: []string{"(", "."},
			},
			SignatureHelpProvider: &lsp.SignatureHelpOptions{
				TriggerCharacters:   []string{"(", ","},
				RetriggerCharacters: []string{"(", ","},
				WorkDoneProgressOptions: lsp.WorkDoneProgressOptions{
					WorkDoneProgress: false,
				},
			},
			DefinitionProvider:              true,
			ReferencesProvider:              true,
			DocumentFormattingProvider:      true,
			DocumentRangeFormattingProvider: true,
			RenameProvider:                  true,
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{Commands: []string{
				CommandExecuteQuery, CommandExplainQuery, CommandGetQueryParameters,
				CommandShowDatabases, CommandShowSchemas, CommandShowConnections,
				CommandSwitchDatabase, CommandSwitchConnection, CommandShowTables,
			}},
		},
	}

	s.stateMu.Lock()
	s.initOptionDBConfig = params.InitializationOptions.ConnectionConfig
	s.stateMu.Unlock()

	// No attachment, metadata, or client messaging is permitted on initialize's
	// read-loop path. Bootstrap is triggered by the initialized notification.
	return result, nil
}

func (s *Server) handleShutdown(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	return nil, s.Stop()
}

func (s *Server) handleExit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	err = s.Stop()
	return nil, err
}

func (s *Server) handleTextDocumentDidOpen(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidOpenTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.openFileAtVersion(params.TextDocument.URI, params.TextDocument.LanguageID, params.TextDocument.Text, params.TextDocument.Version); err != nil {
		return nil, err
	}
	s.queueDiagnosticDocument(params.TextDocument.URI)
	return nil, nil
}

func (s *Server) handleTextDocumentDidChange(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidChangeTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if len(params.ContentChanges) == 0 {
		return nil, nil
	}
	changed, err := s.updateFileVersion(params.TextDocument.URI, params.ContentChanges[0].Text, &params.TextDocument.Version)
	if err != nil {
		return nil, err
	}
	if changed {
		s.queueDiagnosticDocument(params.TextDocument.URI)
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidSave(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params struct {
		Text         *string                    `json:"text"`
		TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	}
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if params.Text != nil {
		revision, revisionErr := s.documentRevision(params.TextDocument.URI)
		if revisionErr != nil {
			return nil, revisionErr
		}
		var applied bool
		applied, err = s.updateFileAtRevision(params.TextDocument.URI, *params.Text, revision)
		if err == nil && applied {
			s.queueDiagnosticDocument(params.TextDocument.URI)
		}
	} else {
		err = s.saveFile(params.TextDocument.URI)
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidClose(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidCloseTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if err := s.closeFile(params.TextDocument.URI); err != nil {
		return nil, err
	}
	s.clearClosedDiagnostics(ctx, conn, params.TextDocument.URI)
	return nil, nil
}

func (s *Server) openFileAtVersion(uri string, languageID string, text string, version int) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.fileRevision++
	f := &File{
		Text:       text,
		LanguageID: languageID,
		Version:    version,
		Revision:   s.fileRevision,
	}
	s.files[uri] = f
	return nil
}

func (s *Server) closeFile(uri string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	delete(s.files, uri)
	return nil
}

func (s *Server) documentRevision(uri string) (uint64, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	f, ok := s.files[uri]
	if !ok {
		return 0, fmt.Errorf("document not found: %v", uri)
	}
	return f.Revision, nil
}

// updateFileAtRevision applies text only while the document is still at the
// revision observed when the save notification began. didSave has no LSP
// version, so this prevents an in-flight save from replacing a newer change.
func (s *Server) updateFileAtRevision(uri string, text string, expectedRevision uint64) (bool, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	f, ok := s.files[uri]
	if !ok {
		return false, fmt.Errorf("document not found: %v", uri)
	}
	if f.Revision != expectedRevision {
		return false, nil
	}
	f.Text = text
	s.fileRevision++
	f.Revision = s.fileRevision
	return true, nil
}

func (s *Server) updateFileVersion(uri string, text string, version *int) (bool, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	f, ok := s.files[uri]
	if !ok {
		return false, fmt.Errorf("document not found: %v", uri)
	}
	if version != nil && *version < f.Version {
		return false, nil
	}
	f.Text = text
	if version != nil {
		f.Version = *version
	}
	s.fileRevision++
	f.Revision = s.fileRevision
	return true, nil
}

func (s *Server) saveFile(uri string) error {
	return nil
}

// fileText returns a copy of the document text for uri. Callers must never
// retain the *File: document update methods mutate it under stateMu, so a reader
// that keeps the pointer races with a concurrent didChange.
func (s *Server) fileText(uri string) (string, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	f, ok := s.files[uri]
	if !ok {
		return "", false
	}
	return f.Text, true
}

func (s *Server) handleWorkspaceDidChangeConfiguration(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	// Update changed configuration
	var params lsp.DidChangeConfigurationParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	s.stateMu.Lock()
	s.WSCfg = params.Settings.SQLS
	initialized := s.initialized
	connected := s.dbConn != nil
	s.stateMu.Unlock()
	if initialized && !connected {
		cfg, index, dbName := s.desiredConnection()
		s.coordinator.Request(s.lifecycleCtx, cfg, index, dbName)
	}
	return nil, nil
}

func (s *Server) handleCancelRequest(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, nil
	}
	var params cancelParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	s.cancels.cancel(params.ID)
	return nil, nil
}

func (s *Server) reconnectionDB(ctx context.Context) error {
	cfg, index, dbName := s.desiredConnection()
	return <-s.coordinator.Request(ctx, cfg, index, dbName)
}

func (s *Server) handleInitialized() {
	s.stateMu.Lock()
	if s.initialized || s.connectionState == connectionStopped {
		s.stateMu.Unlock()
		return
	}
	s.initialized = true
	connected := s.dbConn != nil
	s.stateMu.Unlock()
	if !connected {
		cfg, index, name := s.desiredConnection()
		s.coordinator.Request(s.lifecycleCtx, cfg, index, name)
	}
}

func (s *Server) desiredConnection() (*database.DBConfig, int, string) {
	cfg := s.topConnection()
	s.stateMu.RLock()
	index, name := s.curConnectionIndex, s.curDBName
	s.stateMu.RUnlock()
	if index != 0 {
		cfg = s.getConnection(index)
	}
	if cfg != nil {
		cfg = cloneConnectionConfig(cfg)
		if name != "" {
			cfg.DBName = name
		}
	}
	return cfg, index, name
}

func (s *Server) activeIntentConfig() *database.DBConfig {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return cloneConnectionConfig(s.curDBCfg)
}

func (s *Server) attachIntent(ctx context.Context, intent *connectionIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if intent.Config == nil {
		s.stateMu.Lock()
		s.connectionState = connectionIdle
		s.stateMu.Unlock()
		return ErrNoConnection
	}
	// A queued intent cannot alter the active generation until it owns the
	// write lock, which also serializes it with queries using the old identity.
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := intent.Context.Err(); err != nil {
		return err
	}
	if !s.coordinator.isCurrent(intent) {
		return context.Canceled
	}
	s.diagnosticsPublishMu.Lock()
	s.stateMu.Lock()
	if s.connectionState == connectionStopped {
		s.stateMu.Unlock()
		s.diagnosticsPublishMu.Unlock()
		return context.Canceled
	}
	s.connGeneration++
	generation := s.connGeneration
	s.connectionState = connectionConnecting
	s.metadataStartErr = nil
	old := s.dbConn
	s.dbConn = nil
	s.curDBCfg = cloneConnectionConfig(intent.Config)
	s.curConnectionIndex = intent.ConnectionIndex
	s.curDBName = intent.DatabaseName
	s.ddlMemo = make(map[ddlKey]string)
	s.metadata.Reset(uint64(generation))
	s.stateMu.Unlock()
	s.diagnosticsPublishMu.Unlock()
	_ = s.enqueueDetachedConnection(old)
	s.queueAllDiagnostics()
	candidate, err := s.openConnection(ctx, cloneConnectionConfig(intent.Config))
	if err != nil {
		s.stateMu.Lock()
		if s.connGeneration == generation && s.connectionState != connectionStopped {
			if ctx.Err() != nil || intent.Context.Err() != nil || errors.Is(err, context.Canceled) {
				s.connectionState = connectionIdle
			} else {
				s.connectionState = connectionFailed
			}
		}
		s.stateMu.Unlock()
		return err
	}
	if ctx.Err() != nil || intent.Context.Err() != nil || s.lifecycleCtx.Err() != nil {
		_ = candidate.Close()
		s.stateMu.Lock()
		if s.connGeneration == generation && s.connectionState != connectionStopped {
			s.connectionState = connectionIdle
		}
		s.stateMu.Unlock()
		return context.Canceled
	}
	s.diagnosticsPublishMu.Lock()
	s.stateMu.Lock()
	if s.connGeneration != generation || s.connectionState == connectionStopped || ctx.Err() != nil || intent.Context.Err() != nil || !s.coordinator.isCurrent(intent) {
		if s.connGeneration == generation && s.connectionState != connectionStopped {
			s.connectionState = connectionIdle
		}
		s.stateMu.Unlock()
		s.diagnosticsPublishMu.Unlock()
		_ = candidate.Close()
		return context.Canceled
	}
	s.dbConn = candidate
	s.connectionState = connectionReady
	s.activeConfigKey = intentKey(intent)
	s.stateMu.Unlock()
	s.diagnosticsPublishMu.Unlock()
	repo, err := database.CreateRepositoryFromConnection(intent.Config.Driver, candidate)
	if err == nil {
		_, err = s.metadata.Start(s.lifecycleCtx, uint64(generation), repo)
	}
	if err != nil {
		if markErr := s.metadata.MarkStartFailed(uint64(generation)); markErr != nil {
			log.Printf("sqls: marking metadata generation %d start failure: %v", generation, markErr)
		}
		s.stateMu.Lock()
		if s.connGeneration == generation && s.connectionState == connectionReady {
			s.metadataStartErr = err
		}
		s.stateMu.Unlock()
		log.Printf("sqls: metadata start failed for generation %d: %v", generation, err)
	}
	s.queueAllDiagnostics()
	return nil
}

func (s *Server) cleanupConnections() {
	for conn := range s.cleanupQueue {
		s.closeDetachedConnection(conn)
	}
	s.closeDetachedConnection(<-s.cleanupFinal)
	if s.snapshots != nil {
		s.snapshots.RemoveAll()
	}
	close(s.cleanupDone)
}

func (s *Server) closeDetachedConnection(conn *database.DBConnection) {
	if err := conn.Close(); err != nil {
		log.Printf("sqls: closing database attachment: %v", err)
	}
}

// enqueueDetachedConnection transfers ownership to the cleanup worker. The
// bounded queue normally absorbs close latency; if full, the coordinator is
// backpressured rather than dropping a native attachment handle.
func (s *Server) enqueueDetachedConnection(conn *database.DBConnection) error {
	if conn == nil {
		return nil
	}
	s.cleanupQueue <- conn
	return nil
}

// showConnectionWarnings sends any non-fatal connect-time diagnostics to the
// client. It is a no-op without a connection, without warnings, or without a
// messenger — the two paths that reach reconnectionDB from a command have no
// *jsonrpc2.Conn, and there those warnings are logged only.
func (s *Server) showConnectionWarnings(ctx context.Context, messenger lsp.MessageDisplayer) {
	if messenger == nil {
		return
	}
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if dbConn == nil {
		return
	}
	for _, warning := range dbConn.Warnings {
		if err := messenger.ShowWarning(ctx, warning); err != nil {
			log.Println("send warning", err.Error())
		}
	}
}

func (s *Server) newDBRepository(ctx context.Context) (database.DBRepository, error) {
	s.stateMu.RLock()
	curDBCfg := s.curDBCfg
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if curDBCfg == nil || dbConn == nil {
		return nil, ErrNoConnection
	}
	repo, err := database.CreateRepositoryFromConnection(curDBCfg.Driver, dbConn)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func (s *Server) acquireReadyConnection() (database.DBRepository, func(), error) {
	if !s.connMu.TryRLock() {
		return nil, nil, errConnectionChanging
	}
	s.stateMu.RLock()
	state, hasConnection := s.connectionState, s.dbConn != nil
	s.stateMu.RUnlock()
	if state != connectionReady || !hasConnection {
		s.connMu.RUnlock()
		if !hasConnection && (state == connectionIdle || state == connectionFailed) {
			return nil, nil, errNoReadyConnection
		}
		return nil, nil, errConnectionNotReady
	}
	repo, err := s.newDBRepository(s.lifecycleCtx)
	if err != nil {
		s.connMu.RUnlock()
		return nil, nil, err
	}
	return repo, s.connMu.RUnlock, nil
}

func (s *Server) topConnection() *database.DBConfig {
	// if the init config is set, ignore all other connection configs
	s.stateMu.RLock()
	initCfg := s.initOptionDBConfig
	s.stateMu.RUnlock()
	if initCfg != nil {
		return initCfg
	}

	cfg := s.getConfig()
	if cfg == nil || len(cfg.Connections) == 0 {
		return nil
	}
	return cfg.Connections[0]
}

func (s *Server) getConnection(index int) *database.DBConfig {
	cfg := s.getConfig()
	if cfg == nil || index < 0 || len(cfg.Connections) <= index {
		return nil
	}
	return cfg.Connections[index]
}

func (s *Server) getConfig() *config.Config {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	var cfg *config.Config
	switch {
	case validConfig(s.SpecificFileCfg):
		cfg = s.SpecificFileCfg
	case validConfig(s.WSCfg):
		cfg = s.WSCfg
	case validConfig(s.DefaultFileCfg):
		cfg = s.DefaultFileCfg
	default:
		cfg = config.NewConfig()
	}
	return cfg
}

func (s *Server) connectionConfigsSnapshot() []*database.DBConfig {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	var cfg *config.Config
	switch {
	case validConfig(s.SpecificFileCfg):
		cfg = s.SpecificFileCfg
	case validConfig(s.WSCfg):
		cfg = s.WSCfg
	case validConfig(s.DefaultFileCfg):
		cfg = s.DefaultFileCfg
	default:
		cfg = config.NewConfig()
	}
	connections := make([]*database.DBConfig, len(cfg.Connections))
	for i, connection := range cfg.Connections {
		connections[i] = cloneConnectionConfig(connection)
	}
	return connections
}

// parserDriver returns the active connection's driver. It is retained with its
// exact signature for callers that do not care about the SQL variant.
func (s *Server) parserDriver() dialect.DatabaseDriver {
	return s.parserDriverVariant().Driver
}

// snapshotContext copies the connection identity out from under stateMu.
// Everything the snapshot store does with the result is filesystem I/O, which
// stateMu is never held across.
func (s *Server) snapshotContext() snapshotContext {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	sc := snapshotContext{generation: s.connGeneration, label: "(unnamed)"}
	cfg := s.curDBCfg
	if cfg == nil {
		return sc
	}
	if cfg.Alias != "" {
		sc.label = cfg.Alias
	}
	// Every field that can distinguish one attachment from another goes into
	// the hash. Over-inclusion is free because the result is hashed and never
	// displayed, and it guarantees two different connections never share a
	// snapshot directory.
	sc.identity = fmt.Sprintf("%s|%s|%s|%d|%s|%s",
		cfg.Driver, cfg.DataSourceName, cfg.Host, cfg.Port, cfg.Path, cfg.DBName)
	return sc
}

// parserDriverVariant returns the active connection's driver and its resolved
// server-side SQL variant. With no connection it returns the fully zero
// DriverVariant, and DialectForDriverVariant resolves that to the generic SQL
// dialect because Driver is empty too — not to InterBase's rules. A zero
// Variant field only selects SQL Dialect 3 once Driver is already InterBase.
func (s *Server) parserDriverVariant() dialect.DriverVariant {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.dbConn == nil {
		return dialect.DriverVariant{}
	}
	return s.dbConn.DriverVariant()
}

func validConfig(cfg *config.Config) bool {
	// if cfg != nil && len(cfg.Connections) > 0 {
	if cfg != nil {
		return true
	}
	return false
}
