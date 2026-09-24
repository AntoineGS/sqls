package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestSwitchDatabaseRepositoryErrorReleasesConnectionReadLock(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	unregistered := dialect.DatabaseDriver("handler-unregistered-test-driver")
	s.curDBCfg = &database.DBConfig{Driver: unregistered}
	s.dbConn = &database.DBConnection{Driver: unregistered}
	_, err := s.switchDatabase(context.Background(), lsp.ExecuteCommandParams{Arguments: []interface{}{"denied"}})
	if err == nil {
		t.Fatal("expected repository lookup failure")
	}
	if !s.connMu.TryLock() {
		t.Fatal("switchDatabase repository error leaked connMu.RLock")
	}
	s.connMu.Unlock()
}

const invalidPlanDriver dialect.DatabaseDriver = "handler-invalid-metadata-plan"

type invalidPlanRepository struct{ database.DBRepository }

func (invalidPlanRepository) MetadataPlan() database.MetadataPlan { return database.MetadataPlan{} }
func init() {
	database.RegisterFactory(invalidPlanDriver, func(*sql.DB) database.DBRepository { return invalidPlanRepository{} })
}

func TestMetadataStartRejectionKeepsAttachmentReadyAndRecordsDegradedError(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: invalidPlanDriver}, nil
	}
	err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), &database.DBConfig{Driver: invalidPlanDriver}, 0, ""))
	if err != nil {
		t.Fatalf("attachment result should remain successful after metadata-plan rejection: %v", err)
	}
	s.stateMu.RLock()
	state, startErr := s.connectionState, s.metadataStartErr
	s.stateMu.RUnlock()
	if state != connectionReady {
		t.Fatalf("connection state = %q, want ready", state)
	}
	if !errors.Is(startErr, database.ErrInvalidMetadataPlan) {
		t.Fatalf("metadata start status = %v, want invalid plan error", startErr)
	}
	meta := s.metadata.Snapshot()
	if meta.Started || !meta.StartFailed || !meta.Settled() || !meta.Degraded() {
		t.Fatalf("rejected metadata generation snapshot = %+v (settled=%v degraded=%v)", meta, meta.Settled(), meta.Degraded())
	}
}

func TestConnectionLifecycleRequestCopiesConfig(t *testing.T) {
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Params: map[string]string{"x": "before"}, InterBase: &database.InterBaseConfig{TLS: &database.InterBaseTLSConfig{Enabled: true}}}
	s := NewServer()
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return nil, context.Canceled
	}
	done := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	cfg.Params["x"] = "after"
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not complete request")
	}
	s.Stop()
}

func TestIdenticalNotificationDoesNotQueueDuplicateDuringActiveAttach(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	entered, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		if opens.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("expected only one open")
	}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}
	first := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first attach did not enter opener")
	}
	second := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate config request did not coalesce")
	}
	s.coordinator.mu.Lock()
	pending := s.coordinator.pending
	s.coordinator.mu.Unlock()
	if pending != nil {
		t.Fatal("identical active configuration created pending attach")
	}
	close(release)
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first attach did not finish")
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opener called %d times, want 1", got)
	}
}

func TestExplicitReselectCancelsSameKeyActiveAttach(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	entered := make(chan struct{})
	canceled := make(chan struct{})
	var opens atomic.Int32
	s.openConnection = func(ctx context.Context, _ *database.DBConfig) (*database.DBConnection, error) {
		if opens.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}
		return nil, errors.New("second attach reached opener")
	}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}
	first := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first attach did not enter opener")
	}
	second := s.coordinator.RequestExplicit(s.lifecycleCtx, cfg, 0, "")
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("same-key explicit reselect did not cancel active attach")
	}
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first attach result = %v, want canceled", err)
	}
	if err := <-second; err == nil || err.Error() != "second attach reached opener" {
		t.Fatalf("explicit reselect result = %v, want second opener result", err)
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("opener called %d times, want 2", got)
	}
}

func TestConnectionIntentWaitReturnsOnCancellationWhileWriteLockQueued(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	s.connMu.RLock()
	var opens atomic.Int32
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		opens.Add(1)
		return nil, errors.New("unexpected open")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := s.coordinator.RequestExplicit(ctx, &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3}, 0, "")
	result := make(chan error, 1)
	go func() { result <- awaitConnectionIntent(ctx, done) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled switch waiter remained blocked behind connMu")
	}
	s.connMu.RUnlock()
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.coordinator.done:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop")
	}
	if got := opens.Load(); got != 0 {
		t.Fatalf("opener called %d times after queued cancellation", got)
	}
	if s.connGeneration != 0 {
		t.Fatalf("cancelled queued intent advanced generation to %d", s.connGeneration)
	}
}

func TestCancelledQueuedIntentDoesNotPoisonIdenticalRetry(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	s.connMu.RLock()
	entered := make(chan struct{})
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		close(entered)
		return nil, errors.New("expected retry to run after connMu unlock")
	}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3}
	ctx, cancel := context.WithCancel(context.Background())
	first := s.coordinator.RequestExplicit(ctx, cfg, 0, "")
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first result = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled queued intent did not settle")
	}
	second := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	s.connMu.RUnlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cancelled intent key suppressed an identical later attachment")
	}
	select {
	case err := <-second:
		if err == nil || err.Error() != "expected retry to run after connMu unlock" {
			t.Fatalf("retry result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not settle")
	}
}

func TestDetachedConnectionQueueAppliesBoundedBackpressureWithoutDropping(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	entered, release := make(chan struct{}), make(chan struct{})
	closed := make(chan struct{}, 3)
	first := &database.DBConnection{Conn: blockingCloseDB(t, entered, release, closed)}
	second := &database.DBConnection{Conn: closedSignalDB(t, closed)}
	third := &database.DBConnection{Conn: closedSignalDB(t, closed)}
	fourth := &database.DBConnection{Conn: closedSignalDB(t, closed)}
	if err := s.enqueueDetachedConnection(first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not enter first Close")
	}
	if err := s.enqueueDetachedConnection(second); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueDetachedConnection(third); err != nil {
		t.Fatal(err)
	}
	attempting := make(chan struct{})
	queued := make(chan error, 1)
	go func() {
		close(attempting)
		queued <- s.enqueueDetachedConnection(fourth)
	}()
	select {
	case <-attempting:
	case <-time.After(time.Second):
		t.Fatal("fourth cleanup handoff did not start")
	}
	select {
	case err := <-queued:
		t.Fatalf("full bounded queue must backpressure, enqueue returned %v before capacity", err)
	case <-time.After(time.Second): // watchdog: release is the only event that frees queue capacity.
	}
	close(release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("third close was dropped or never enqueued")
	}
	for range 4 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("cleanup worker lost a detached connection")
		}
	}
}

type cleanupConnector func(context.Context) (driver.Conn, error)

func (c cleanupConnector) Connect(ctx context.Context) (driver.Conn, error) { return c(ctx) }
func (cleanupConnector) Driver() driver.Driver                              { return cleanupDriver{} }

type cleanupDriver struct{}

func (cleanupDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

type cleanupConn struct{ closeFn func() error }

func (c *cleanupConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (c *cleanupConn) Close() error                        { return c.closeFn() }
func (c *cleanupConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }

func blockingCloseDB(t *testing.T, entered chan struct{}, release <-chan struct{}, closed chan<- struct{}) *sql.DB {
	t.Helper()
	db := sql.OpenDB(cleanupConnector(func(context.Context) (driver.Conn, error) {
		return &cleanupConn{closeFn: func() error { close(entered); <-release; closed <- struct{}{}; return nil }}, nil
	}))
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}
func closedSignalDB(t *testing.T, closed chan<- struct{}) *sql.DB {
	t.Helper()
	db := sql.OpenDB(cleanupConnector(func(context.Context) (driver.Conn, error) {
		return &cleanupConn{closeFn: func() error { closed <- struct{}{}; return nil }}, nil
	}))
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInitializeDoesNotWaitForConnectionOpen(t *testing.T) {
	s := NewServer()
	entered, release := make(chan struct{}), make(chan struct{})
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		close(entered)
		select {
		case <-release:
			return nil, context.Canceled
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	clientPipe, serverPipe := net.Pipe()
	serverConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(s.Handle))
	clientConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	defer clientConn.Close()
	defer serverConn.Close()
	defer close(release)
	defer s.Stop()
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}
	init := lsp.InitializeParams{InitializationOptions: lsp.InitializeOptions{ConnectionConfig: cfg}}
	callDone := make(chan error, 1)
	go func() { callDone <- clientConn.Call(context.Background(), "initialize", init, nil) }()
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize waited for connection work")
	}
	select {
	case <-entered:
		t.Fatal("connection started before initialized")
	default:
	}
	if err := clientConn.Notify(context.Background(), "initialized", struct{}{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not start")
	}
	if err := clientConn.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: "file:///test.sql", LanguageID: "sql", Text: "select 1", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	formatDone := make(chan error, 1)
	go func() {
		formatDone <- clientConn.Call(context.Background(), "textDocument/formatting", lsp.DocumentFormattingParams{TextDocument: lsp.TextDocumentIdentifier{URI: "file:///test.sql"}}, nil)
	}()
	select {
	case err := <-formatDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("formatting blocked behind connection open")
	}
}

func TestStopDuringAttachedOpenReturnsAndClosesLateCandidate(t *testing.T) {
	s := NewServer()
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		close(entered)
		<-release
		return &database.DBConnection{Driver: dialect.DatabaseDriverSQLite3, Conn: closedSignalDB(t, closed)}, nil
	}
	clientPipe, serverPipe := net.Pipe()
	serverConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(s.Handle))
	clientConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	defer clientConn.Close()
	defer serverConn.Close()
	params := lsp.InitializeParams{InitializationOptions: lsp.InitializeOptions{ConnectionConfig: &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}}}
	if err := clientConn.Call(context.Background(), "initialize", params, nil); err != nil {
		t.Fatal(err)
	}
	if err := clientConn.Notify(context.Background(), "initialized", struct{}{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("attach did not enter opener")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- s.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop waited for blocked opener")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("late candidate was not closed locally")
	}
	select {
	case <-s.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not complete after opener was released")
	}
	s.stateMu.RLock()
	installed := s.dbConn
	state := s.connectionState
	s.stateMu.RUnlock()
	if installed != nil || state != connectionStopped {
		t.Fatalf("late candidate installed: conn=%p state=%q", installed, state)
	}
}

func TestCancelledActiveIntentClosesCandidateAndLeavesTerminalIdleState(t *testing.T) {
	s := NewServer()
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		close(entered)
		<-release
		return &database.DBConnection{Conn: closedSignalDB(t, closed)}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reply := s.coordinator.RequestExplicit(ctx, &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3}, 0, "")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("active attach did not enter opener")
	}
	cancel()
	close(release)
	select {
	case err := <-reply:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("intent result = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled active attach did not settle")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled candidate was not closed")
	}
	s.stateMu.RLock()
	state, installed := s.connectionState, s.dbConn
	s.stateMu.RUnlock()
	if state != connectionIdle || installed != nil {
		t.Fatalf("cancelled attach left state=%q connection=%p, want idle and detached", state, installed)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
}

func TestDatabaseCommandsDuringConnectionTransitionReturnPromptly(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server) error
	}{
		{name: "explain", call: func(s *Server) error {
			_, err := s.explainQuery(context.Background(), lsp.ExecuteCommandParams{})
			return err
		}},
		{name: "query parameters", call: func(s *Server) error {
			_, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{})
			return err
		}},
		{name: "switch database validation", call: func(s *Server) error {
			_, err := s.switchDatabase(context.Background(), lsp.ExecuteCommandParams{Arguments: []interface{}{"next"}})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer()
			s.stateMu.Lock()
			s.connectionState = connectionConnecting
			s.stateMu.Unlock()
			s.connMu.Lock() // models an attach holding the transition write lock.
			result := make(chan error, 1)
			go func() { result <- tc.call(s) }()
			select {
			case err := <-result:
				s.connMu.Unlock()
				if err == nil || !strings.Contains(err.Error(), "database connection") {
					t.Fatalf("command error = %v, want a clear transient connection error", err)
				}
			case <-time.After(time.Second):
				s.connMu.Unlock()
				t.Fatal("command blocked behind an attachment instead of returning transient readiness error")
			}
			if err := s.Stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShowConnectionsDoesNotWaitForConnectionTransition(t *testing.T) {
	s := NewServer()
	s.connMu.Lock()
	result := make(chan error, 1)
	go func() { _, err := s.showConnections(context.Background(), lsp.ExecuteCommandParams{}); result <- err }()
	select {
	case err := <-result:
		s.connMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.connMu.Unlock()
		t.Fatal("showConnections blocked on connMu")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryParametersTreatsLockContentionAsTransitionWithoutConnection(t *testing.T) {
	s := NewServer()
	s.connMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{})
		result <- err
	}()
	select {
	case err := <-result:
		s.connMu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "database connection is changing") {
			t.Fatalf("getQueryParameters error = %v, want transient lock-contention error", err)
		}
	case <-time.After(time.Second):
		s.connMu.Unlock()
		t.Fatal("getQueryParameters waited on connMu")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLateWorkspaceConfigurationBootstrapsAfterInitializedOnce(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	entered, release := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		if opens.Add(1) != 1 {
			return nil, errors.New("duplicate late-config attachment")
		}
		close(entered)
		select {
		case <-release:
			return nil, errors.New("test opener failure")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.handleInitialized() // no config yet: bootstrap settles without opening.

	cfg := &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}}}
	raw, err := json.Marshal(map[string]interface{}{"settings": map[string]interface{}{"sqls": cfg}})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(raw)
	_, err = s.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &params})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("late workspace configuration did not start attachment")
	}
	s.handleInitialized()
	s.handleInitialized()
	close(release)
	deadline := time.After(time.Second)
	for {
		s.stateMu.RLock()
		state := s.connectionState
		s.stateMu.RUnlock()
		if state == connectionFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("late configuration never reached terminal failure state; got %q", state)
		default:
			runtime.Gosched()
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("openConnection called %d times, want 1 after repeated initialized", got)
	}
}

func TestChangedConfigurationSupersedesActiveAttachAndClosesStaleCandidate(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	enteredA, releaseA, staleClosed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	enteredB, installed := make(chan struct{}), &database.DBConnection{Driver: dialect.DatabaseDriverSQLite3}
	var opens atomic.Int32
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		if opens.Add(1) == 1 {
			close(enteredA)
			<-releaseA // deliberately ignore cancellation and return a late candidate.
			return &database.DBConnection{Conn: closedSignalDB(t, staleClosed)}, nil
		}
		close(enteredB)
		return installed, nil
	}
	cfgA := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: "A"}
	cfgB := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: "B"}
	first := s.coordinator.Request(s.lifecycleCtx, cfgA, 0, "")
	select {
	case <-enteredA:
	case <-time.After(time.Second):
		t.Fatal("A attach did not start")
	}
	second := s.coordinator.Request(s.lifecycleCtx, cfgB, 0, "")
	close(releaseA)
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("superseded A result = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("superseded A did not settle")
	}
	select {
	case <-staleClosed:
	case <-time.After(time.Second):
		t.Fatal("stale A candidate was not closed locally")
	}
	select {
	case <-enteredB:
	case <-time.After(time.Second):
		t.Fatal("new B configuration was not attached")
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("B attach did not settle")
	}
	s.stateMu.RLock()
	got, state := s.dbConn, s.connectionState
	s.stateMu.RUnlock()
	if got != installed || state != connectionReady {
		t.Fatalf("installed connection=%p state=%q, want B=%p ready", got, state, installed)
	}
}

func TestSwitchConnectionsRejectsOutOfRangeBeforeSubmittingIntent(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	active := &database.DBConnection{Driver: dialect.DatabaseDriverSQLite3}
	s.SpecificFileCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}}}
	s.stateMu.Lock()
	s.dbConn = active
	s.curDBCfg = cloneConnectionConfig(s.SpecificFileCfg.Connections[0])
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	called := atomic.Bool{}
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		called.Store(true)
		return nil, errors.New("unexpected opener call")
	}
	_, err := s.switchConnections(context.Background(), lsp.ExecuteCommandParams{Arguments: []interface{}{"2"}})
	if err == nil {
		t.Fatal("out-of-range connection index was accepted")
	}
	s.stateMu.RLock()
	gotConn, gotState := s.dbConn, s.connectionState
	s.stateMu.RUnlock()
	if gotConn != active || gotState != connectionReady {
		t.Fatalf("after rejected switch: conn=%p state=%q, want preserved active %p ready", gotConn, gotState, active)
	}
	if called.Load() {
		t.Fatal("out-of-range switch reached connection opener")
	}
}

func TestRequestDuringCoordinatorClaimCancelsClaimedIntent(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	claimed, continueClaim := make(chan struct{}), make(chan struct{})
	var hookOnce atomic.Bool
	s.coordinator.mu.Lock()
	s.coordinator.afterClaim = func() {
		if hookOnce.CompareAndSwap(false, true) {
			close(claimed)
			<-continueClaim
		}
	}
	s.coordinator.mu.Unlock()
	var opens atomic.Int32
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		opens.Add(1)
		return &database.DBConnection{Driver: dialect.DatabaseDriverSQLite3}, nil
	}
	cfgA := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: "claim-A"}
	cfgB := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: "claim-B"}
	a := s.coordinator.Request(context.Background(), cfgA, 0, "")
	select {
	case <-claimed:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not claim A")
	}
	b := s.coordinator.Request(context.Background(), cfgB, 0, "")
	close(continueClaim)
	select {
	case err := <-a:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("A result = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("claimed A did not settle")
	}
	select {
	case err := <-b:
		if err != nil {
			t.Fatalf("B result = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("latest B intent did not settle")
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("openConnection calls = %d, want only latest intent", got)
	}
	s.stateMu.RLock()
	installed := s.curDBCfg
	s.stateMu.RUnlock()
	if installed == nil || installed.DataSourceName != "claim-B" {
		t.Fatalf("installed config = %+v, want B", installed)
	}
}

func TestCloneConnectionConfigDeepCopiesNestedSettings(t *testing.T) {
	cfg := &database.DBConfig{Params: map[string]string{"a": "b"}, SSHCfg: &database.SSHConfig{Host: "host"}, InterBase: &database.InterBaseConfig{TLS: &database.InterBaseTLSConfig{Enabled: true}}}
	clone := cloneConnectionConfig(cfg)
	clone.Params["a"] = "c"
	clone.SSHCfg.Host = "other"
	clone.InterBase.TLS.Enabled = false
	if cfg.Params["a"] != "b" || cfg.SSHCfg.Host != "host" || !cfg.InterBase.TLS.Enabled {
		t.Fatal("clone shares mutable config state")
	}
}
