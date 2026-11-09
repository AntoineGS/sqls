package handler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

type serverCheckFunc func(context.Context, string) (*language.StatementError, error)

func (f serverCheckFunc) CheckStatement(ctx context.Context, sql string) (*language.StatementError, error) {
	return f(ctx, sql)
}

type serverCheckTimerRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
	timers []*time.Timer
}

func (r *serverCheckTimerRecorder) after(delay time.Duration, _ func()) *time.Timer {
	timer := time.NewTimer(time.Hour)
	r.mu.Lock()
	r.delays = append(r.delays, delay)
	r.timers = append(r.timers, timer)
	r.mu.Unlock()
	return timer
}

func (r *serverCheckTimerRecorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, timer := range r.timers {
		timer.Stop()
	}
}

func TestServerCheckDidChangeDebouncesOneTimerPerDocument(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-debounce.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1, Revision: 1}
	tx.server.stateMu.Unlock()
	recorder := &serverCheckTimerRecorder{}
	tx.server.serverCheck.mu.Lock()
	tx.server.serverCheck.after = recorder.after
	tx.server.serverCheck.mu.Unlock()
	t.Cleanup(recorder.close)

	tx.change(t, uri, "SELECT 2", 2)
	tx.change(t, uri, "SELECT 3", 3)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.delays) != 2 || recorder.delays[0] != 750*time.Millisecond || recorder.delays[1] != 750*time.Millisecond {
		t.Fatalf("scheduled server-check delays = %v, want two 750ms delays", recorder.delays)
	}
	if recorder.timers[0].Stop() {
		t.Fatal("first timer was not stopped when the second change re-armed the document")
	}
	tx.server.serverCheck.mu.Lock()
	defer tx.server.serverCheck.mu.Unlock()
	if len(tx.server.serverCheck.timers) != 1 || tx.server.serverCheck.timers[uri] != recorder.timers[1] {
		t.Fatalf("pending timers = %v, want only the newest timer for %s", tx.server.serverCheck.timers, uri)
	}
}

func TestServerCheckDidOpenAndDidSaveScheduleImmediately(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-immediate.sql"
	recorder := &serverCheckTimerRecorder{}
	tx.server.serverCheck.mu.Lock()
	tx.server.serverCheck.after = recorder.after
	tx.server.serverCheck.mu.Unlock()
	t.Cleanup(recorder.close)

	tx.open(t, uri, "SELECT 1", 1)
	tx.call(t, "textDocument/didSave", lsp.DidSaveTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	})

	recorder.mu.Lock()
	if len(recorder.delays) != 2 || recorder.delays[0] != 0 || recorder.delays[1] != 0 {
		delays := append([]time.Duration(nil), recorder.delays...)
		recorder.mu.Unlock()
		t.Fatalf("didOpen/didSave server-check delays = %v, want [0 0]", delays)
	}
	timer := recorder.timers[1]
	recorder.mu.Unlock()
	tx.call(t, "textDocument/didClose", lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	})
	if timer.Stop() {
		t.Fatal("didClose did not stop its pending server-check timer")
	}
	tx.server.serverCheck.mu.Lock()
	defer tx.server.serverCheck.mu.Unlock()
	if len(tx.server.serverCheck.timers) != 0 {
		t.Fatalf("pending timers after didClose = %v, want none", tx.server.serverCheck.timers)
	}
}

func TestServerCheckStopStopsPendingTimers(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-stop.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1, Revision: 1}
	tx.server.stateMu.Unlock()
	recorder := &serverCheckTimerRecorder{}
	tx.server.serverCheck.mu.Lock()
	tx.server.serverCheck.after = recorder.after
	tx.server.serverCheck.mu.Unlock()
	t.Cleanup(recorder.close)
	tx.server.scheduleServerCheck(uri, time.Hour)

	recorder.mu.Lock()
	timer := recorder.timers[0]
	recorder.mu.Unlock()
	if err := tx.server.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if timer.Stop() {
		t.Fatal("Stop() did not stop the pending server-check timer")
	}
	tx.server.serverCheck.mu.Lock()
	defer tx.server.serverCheck.mu.Unlock()
	if len(tx.server.serverCheck.timers) != 0 {
		t.Fatalf("pending timers after Stop() = %v, want none", tx.server.serverCheck.timers)
	}
}

func TestServerCheckChangedDocumentDoesNotQueueStaleRun(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-stale.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1, Revision: 1}
	tx.server.stateMu.Unlock()

	started := make(chan struct{}, 1)
	checker := serverCheckFunc(func(ctx context.Context, _ string) (*language.StatementError, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tx.server.stateMu.Lock()
	tx.server.serverCheckerOverride = func() language.StatementChecker { return checker }
	tx.server.stateMu.Unlock()
	done := make(chan struct{})
	go func() {
		tx.server.runServerCheck(uri)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server checker did not start")
	}

	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 2", Version: 2, Revision: 2}
	tx.server.stateMu.Unlock()
	recorder := &serverCheckTimerRecorder{}
	tx.server.serverCheck.mu.Lock()
	tx.server.serverCheck.after = recorder.after
	tx.server.serverCheck.mu.Unlock()
	tx.server.scheduleServerCheck(uri, time.Hour)
	t.Cleanup(recorder.close)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("document edit did not cancel the in-flight server check")
	}
	tx.server.diagnosticWorkMu.Lock()
	_, queued := tx.server.diagnosticDocuments[uri]
	tx.server.diagnosticWorkMu.Unlock()
	if queued {
		t.Fatal("stale server-check completion queued a diagnostics publish")
	}
}

func TestServerCheckPublishMergesCacheAndDropsEditedStatementFinding(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-cache.sql"
	text := "SELECT ID FROM T WHERE V = NULL;\n_SELECT 1 FROM RDB$DATABASE;"
	tx.server.stateMu.Lock()
	tx.server.notificationConn = tx.serverConn
	tx.server.files[uri] = &File{Text: text, Version: 1, Revision: 1}
	tx.server.serverCheckerOverride = func() language.StatementChecker {
		return serverCheckFunc(func(_ context.Context, sql string) (*language.StatementError, error) {
			if strings.Contains(sql, "_SELECT") {
				return &language.StatementError{SQLCode: -104, Message: "Token unknown: _SELECT", Line: 1, Column: 1, Token: "_SELECT"}, nil
			}
			return nil, nil
		})
	}
	tx.server.stateMu.Unlock()

	done := make(chan struct{})
	go func() {
		tx.server.runServerCheck(uri)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server check did not finish")
	}
	first := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	codes := make(map[string]bool)
	for _, diagnostic := range first.Diagnostics {
		codes[diagnosticCode(diagnostic)] = true
	}
	if !codes["interbase-null-comparison"] || !codes["interbase-syntax"] {
		t.Fatalf("diagnostics = %+v, want static null-comparison and cached server syntax findings", first.Diagnostics)
	}

	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT ID FROM T WHERE V = NULL;\n_SELECT 2 FROM RDB$DATABASE;", Version: 2, Revision: 2}
	tx.server.stateMu.Unlock()
	tx.server.languageService.InvalidateDocument(uri)
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	second := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	for _, diagnostic := range second.Diagnostics {
		if diagnosticCode(diagnostic) == "interbase-syntax" {
			t.Fatalf("edited statement retained stale server finding: %+v", second.Diagnostics)
		}
	}
	if len(second.Diagnostics) == 0 || diagnosticCode(second.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics after editing server-invalid statement = %+v, want only static null-comparison", second.Diagnostics)
	}
}

func TestServerCheckDisabledSkipsSchedulingAndCachedMerge(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///server-check-disabled.sql"
	text := "_SELECT 1 FROM RDB$DATABASE;"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: text, Version: 1, Revision: 1}
	tx.server.stateMu.Unlock()
	snapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("diagnosticsSnapshot() failed")
	}
	checker := serverCheckFunc(func(context.Context, string) (*language.StatementError, error) {
		return &language.StatementError{SQLCode: -104, Message: "Token unknown: _SELECT", Line: 1, Column: 1, Token: "_SELECT"}, nil
	})
	if _, err := tx.server.languageService.ServerDiagnostics(tx.ctx, languageRequest(editorSnapshot{
		URI: snapshot.uri, Text: snapshot.text, Version: snapshot.version, Revision: snapshot.revision,
		Generation: snapshot.generation, Variant: snapshot.variant, DialectResolved: snapshot.dialectResolved,
		CacheEpoch: snapshot.cacheEpoch, Metadata: snapshot.metadata, MetadataKey: snapshot.metadataKey,
		MetadataLease: snapshot.metadataLease, MetadataIncarnation: snapshot.metadataIncarnation,
		MetadataGeneration: snapshot.metadataGeneration, DiagnosticPolicy: snapshot.policy,
		PolicyRevision: snapshot.policyRevision,
	}), checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("warm server diagnostics cache: %v", err)
	}

	disabled := false
	tx.server.stateMu.Lock()
	tx.server.WSCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{ServerCheck: config.ServerCheckConfig{Enabled: &disabled}}}
	tx.server.stateMu.Unlock()
	recorder := &serverCheckTimerRecorder{}
	tx.server.serverCheck.mu.Lock()
	tx.server.serverCheck.after = recorder.after
	tx.server.serverCheck.mu.Unlock()
	t.Cleanup(recorder.close)
	tx.server.scheduleServerCheck(uri, 0)
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	got := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil })
	for _, diagnostic := range got.Diagnostics {
		if diagnosticCode(diagnostic) == "interbase-syntax" {
			t.Fatalf("disabled server check published cached server finding: %+v", got.Diagnostics)
		}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.delays) != 0 {
		t.Fatalf("disabled server check armed %d timers, want none", len(recorder.delays))
	}
}

func TestServerCheckNonInterBaseDoesNotCallChecker(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverMySQL)
	const uri = "file:///server-check-mysql.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1, Revision: 1}
	var calls int
	tx.server.serverCheckerOverride = func() language.StatementChecker {
		return serverCheckFunc(func(context.Context, string) (*language.StatementError, error) {
			calls++
			return nil, nil
		})
	}
	tx.server.stateMu.Unlock()

	tx.server.runServerCheck(uri)
	if calls != 0 {
		t.Fatalf("non-InterBase checker calls = %d, want 0", calls)
	}
}

func TestServerCheckCheckerFailureDoesNotQueuePublish(t *testing.T) {
	const uri = "file:///server-check-failure.sql"
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previousOutput)
	s := &Server{
		files:               map[string]*File{uri: {Text: "SELECT 1", Version: 1, Revision: 1}},
		languageService:     language.NewService(),
		serverCheck:         newServerCheckState(),
		lifecycleCtx:        context.Background(),
		dbConn:              &database.DBConnection{Driver: dialect.DatabaseDriverInterBase},
		diagnosticDocuments: make(map[string]struct{}),
		diagnosticsWake:     make(chan struct{}, 1),
	}
	s.serverCheckerOverride = func() language.StatementChecker {
		return serverCheckFunc(func(context.Context, string) (*language.StatementError, error) {
			return nil, errors.New("connection lost")
		})
	}

	s.runServerCheck(uri)
	s.diagnosticWorkMu.Lock()
	_, queued := s.diagnosticDocuments[uri]
	s.diagnosticWorkMu.Unlock()
	if queued {
		t.Fatal("checker failure queued a diagnostics publish instead of aborting the run")
	}
	if !strings.Contains(output.String(), "sqls: server check skipped for "+uri+": connection lost") {
		t.Fatalf("checker failure log = %q, want URI and connection error", output.String())
	}
}
