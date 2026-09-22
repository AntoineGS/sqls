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
)

var (
	ErrNoConnection = errors.New("no database connection")
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
	connGeneration int

	// ddlMemo caches the rendered DDL appendix per connection generation.
	// Hover fires on every cursor rest over the same token; without this,
	// each one is a catalog round trip. Guarded by stateMu, and never held
	// across the round trip itself.
	ddlMemo map[ddlKey]string

	// snapshots materialises database-resident source as read-only files. It is
	// nil when the user cache directory could not be located, which disables
	// go-to-definition for database objects and nothing else.
	snapshots *sourceSnapshotStore

	worker  *database.Worker
	files   map[string]*File
	cancels *cancelRegistry
}

type File struct {
	LanguageID string
	Text       string
}

func NewServer() *Server {
	worker := database.NewWorker()
	worker.Start()

	server := &Server{
		files:   make(map[string]*File),
		ddlMemo: make(map[ddlKey]string),
		worker:  worker,
		cancels: newCancelRegistry(),
	}
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
	defer s.snapshots.RemoveAll()
	defer s.worker.Stop()
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	return dbConn.Close()
}

func (s *Server) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
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
		},
	}

	s.stateMu.Lock()
	s.initOptionDBConfig = params.InitializationOptions.ConnectionConfig
	s.stateMu.Unlock()

	// Initialize database database connection
	// NOTE: If no connection is found at this point, it is possible that the connection settings are sent to workspace config, so don't make an error
	messenger := lsp.NewMessenger(conn)
	s.connMu.Lock()
	err = s.reconnectionDB(ctx)
	s.connMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}
	s.showConnectionWarnings(ctx, messenger)
	return result, nil
}

func (s *Server) handleShutdown(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if dbConn != nil {
		dbConn.Close()
	}
	return nil, nil
}

func (s *Server) handleExit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if dbConn != nil {
		dbConn.Close()
	}
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

	if err := s.openFile(params.TextDocument.URI, params.TextDocument.LanguageID); err != nil {
		return nil, err
	}
	if err := s.updateFile(params.TextDocument.URI, params.TextDocument.Text); err != nil {
		return nil, err
	}
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
	if err := s.updateFile(params.TextDocument.URI, params.ContentChanges[0].Text); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) handleTextDocumentDidSave(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DidSaveTextDocumentParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if params.Text != "" {
		err = s.updateFile(params.TextDocument.URI, params.Text)
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
	return nil, nil
}

func (s *Server) openFile(uri string, languageID string) error {
	f := &File{
		Text:       "",
		LanguageID: languageID,
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.files[uri] = f
	return nil
}

func (s *Server) closeFile(uri string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	delete(s.files, uri)
	return nil
}

func (s *Server) updateFile(uri string, text string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	f, ok := s.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text
	return nil
}

func (s *Server) saveFile(uri string) error {
	return nil
}

// fileText returns a copy of the document text for uri. Callers must never
// retain the *File: updateFile mutates Text through the stored pointer, so a
// reader that keeps the pointer races with a concurrent didChange.
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
	s.stateMu.Unlock()

	// Skip database connection
	s.stateMu.RLock()
	connected := s.dbConn != nil
	s.stateMu.RUnlock()
	if connected {
		return nil, nil
	}

	// Initialize database database connection
	messenger := lsp.NewMessenger(conn)
	s.connMu.Lock()
	err = s.reconnectionDB(ctx)
	s.connMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}
	s.showConnectionWarnings(ctx, messenger)

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
	s.stateMu.RLock()
	oldConn := s.dbConn
	s.stateMu.RUnlock()
	if err := oldConn.Close(); err != nil {
		return err
	}

	dbConn, err := s.newDBConnection(ctx)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	s.dbConn = dbConn
	s.connGeneration++
	// The new connection may be a different database entirely, so nothing
	// cached against the old one is still true.
	s.ddlMemo = make(map[ddlKey]string)
	s.stateMu.Unlock()

	for _, warning := range dbConn.Warnings {
		log.Println(warning)
	}

	dbRepo, err := s.newDBRepository(ctx)
	if err != nil {
		return err
	}
	if err := s.worker.ReCache(ctx, dbRepo); err != nil {
		return err
	}
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

func (s *Server) newDBConnection(ctx context.Context) (*database.DBConnection, error) {
	// Get the most preferred DB connection settings
	connCfg := s.topConnection()
	if connCfg == nil {
		return nil, ErrNoConnection
	}
	s.stateMu.RLock()
	index := s.curConnectionIndex
	dbName := s.curDBName
	s.stateMu.RUnlock()

	if index != 0 {
		connCfg = s.getConnection(index)
	}
	if connCfg == nil {
		return nil, fmt.Errorf("not found database connection config, index %d", index+1)
	}
	if dbName != "" {
		connCfg.DBName = dbName
	}
	s.stateMu.Lock()
	s.curDBCfg = connCfg
	s.stateMu.Unlock()

	// Connect database
	conn, err := database.Open(connCfg)
	if err != nil {
		return nil, err
	}
	return conn, nil
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
