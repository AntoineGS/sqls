package handler

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// --- raw JSON diagnostics array shape: never null, per the Neovim-crash fix ---

func TestDiagnosticsRawJSONNeverNull(t *testing.T) {
	const uri = "file:///raw-json-array.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, "SELECT 1", 1)
	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if string(notification.rawDiagnostics) != "[]" {
		t.Fatalf("raw diagnostics JSON = %s, want [] (not null)", notification.rawDiagnostics)
	}

	tx.call(t, "textDocument/didClose", lsp.DidCloseTextDocumentParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}})
	closed := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version == nil })
	if string(closed.rawDiagnostics) != "[]" {
		t.Fatalf("close raw diagnostics JSON = %s, want [] (not null)", closed.rawDiagnostics)
	}
}

// --- exact UTF-16 span for a new-task (null-comparison) finding ---

func TestDiagnosticsExactUTF16SpanForNullComparisonFinding(t *testing.T) {
	const uri = "file:///utf16-span.sql"
	text := "/*😀*/ SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(notification.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics = %+v, want interbase-null-comparison", notification.Diagnostics)
	}
	expr := "V = NULL"
	start := strings.Index(text, expr)
	wantStart := utf16Units(text[:start])
	wantEnd := wantStart + utf16Units(expr)
	got := notification.Diagnostics[0].Range
	if got.Start != (lsp.Position{Character: wantStart}) || got.End != (lsp.Position{Character: wantEnd}) {
		t.Fatalf("range = %+v, want character range %d..%d after the supplementary-plane rune", got, wantStart, wantEnd)
	}
}

// --- one document with both a top-level unknown column (Task 4) and a
// procedural unknown local (Task 3), verifying the two rule groups coexist ---

func TestDiagnosticsUnknownColumnAndProceduralUnknownVariableCoexist(t *testing.T) {
	const uri = "file:///coexist.sql"
	text := "SELECT MISSING_COL FROM T;\nCREATE PROCEDURE P AS\nBEGIN\n  UNKNOWN_VAR = 1;\nEND"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	columns := []*database.ColumnDesc{{ColumnBase: database.ColumnBase{Schema: "TEST", Table: "T", Name: "ID"}, Type: "INTEGER"}}
	repo := &database.MockDBRepository{
		MockDatabase:       func(context.Context) (string, error) { return "TEST", nil },
		MockDatabases:      func(context.Context) ([]string, error) { return []string{"TEST"}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) { return map[string][]string{"TEST": {"T"}}, nil },
		MockDescribeDatabaseTable: func(context.Context) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	loadMetadataForTest(t, tx.server, repo)
	tx.open(t, uri, text, 1)

	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 2
	})
	byCode := make(map[string]lsp.Diagnostic, len(notification.Diagnostics))
	for _, d := range notification.Diagnostics {
		byCode[diagnosticCode(d)] = d
	}
	if _, ok := byCode["interbase-unknown-column"]; !ok {
		t.Fatalf("diagnostics = %+v, missing top-level interbase-unknown-column", notification.Diagnostics)
	}
	if _, ok := byCode["interbase-unknown-variable"]; !ok {
		t.Fatalf("diagnostics = %+v, missing procedural interbase-unknown-variable", notification.Diagnostics)
	}
}

func TestDiagnosticExpansionMixedDocumentKeepsIndependentFindingsAndCloseReopen(t *testing.T) {
	const uri = "file:///diagnostic-expansion-mixed.sql"
	text := "/*😀*/ SELECT ID FROM T WHERE V = NULL;\n" +
		"SELECT FROM;\n" +
		"INSERT INTO T (ID) SELECT ID FROM T WHERE V = NULL;\n" +
		"UPDATE T SET ID = 1 WHERE V = NULL;\n" +
		"CREATE PROCEDURE P AS BEGIN BAD_PROC = 1; END\n" +
		"CREATE TRIGGER TR FOR T BEFORE INSERT AS BEGIN BAD_TRIGGER = 1; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1
	})
	if string(initial.rawDiagnostics) == "null" || string(initial.rawDiagnostics) == "" || initial.rawDiagnostics[0] != '[' {
		t.Fatalf("wire diagnostics payload = %s, want a JSON array", initial.rawDiagnostics)
	}
	want := []struct {
		code  string
		text  string
		line  int
		start int
	}{
		{code: "interbase-null-comparison", text: "V = NULL", line: 0, start: 30},
		{code: "interbase-null-comparison", text: "V = NULL", line: 2, start: 42},
		{code: "interbase-null-comparison", text: "V = NULL", line: 3, start: 26},
		{code: "interbase-unknown-variable", text: "BAD_PROC", line: 4, start: 28},
		{code: "interbase-unknown-variable", text: "BAD_TRIGGER", line: 5, start: 47},
	}
	if len(initial.Diagnostics) != len(want) {
		t.Fatalf("findings = %+v, want exactly %d independent findings (the incomplete neighbor and unproven catalog claims stay silent)", initial.Diagnostics, len(want))
	}
	for _, expected := range want {
		var match *lsp.Diagnostic
		for index := range initial.Diagnostics {
			finding := &initial.Diagnostics[index]
			if diagnosticCode(*finding) == expected.code && finding.Range.Start.Line == expected.line && finding.Range.Start.Character == expected.start {
				if match != nil {
					t.Fatalf("multiple diagnostics match expected %s at line %d character %d", expected.code, expected.line, expected.start)
				}
				match = finding
			}
		}
		if match == nil {
			t.Fatalf("findings = %+v, missing %s over %q at line %d character %d", initial.Diagnostics, expected.code, expected.text, expected.line, expected.start)
		}
		lineText := strings.Split(text, "\n")[expected.line]
		if expected.start < 0 || expected.start+utf16Units(expected.text) > utf16Units(lineText) {
			t.Fatalf("invalid expected range for %q on line %d", expected.text, expected.line)
		}
		wantRange := lsp.Range{
			Start: lsp.Position{Line: expected.line, Character: expected.start},
			End:   lsp.Position{Line: expected.line, Character: expected.start + utf16Units(expected.text)},
		}
		if match.Range != wantRange {
			t.Fatalf("%s range = %+v, want exact UTF-16 range %+v for %q", expected.code, match.Range, wantRange, expected.text)
		}
	}
	for _, finding := range initial.Diagnostics {
		if finding.Range.Start.Line == 1 {
			t.Fatalf("incomplete neighboring statement produced a finding: %+v", finding)
		}
	}

	tx.call(t, "textDocument/didClose", lsp.DidCloseTextDocumentParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}})
	closed := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version == nil })
	if string(closed.rawDiagnostics) != "[]" {
		t.Fatalf("close diagnostics wire payload = %s, want []", closed.rawDiagnostics)
	}
	tx.open(t, uri, "SELECT 1;", 2)
	reopened := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	if len(reopened.Diagnostics) != 0 || string(reopened.rawDiagnostics) != "[]" {
		t.Fatalf("reopened clean document notification = %+v (%s), want empty []", reopened.Diagnostics, reopened.rawDiagnostics)
	}
}

func TestDiagnosticExpansionSlowOldVersionDoesNotBlockEditorRequestsOrPublish(t *testing.T) {
	const uri = "file:///diagnostic-expansion-version.sql"
	const otherURI = "file:///diagnostic-expansion-other.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseGate)
	var block atomic.Bool
	var once sync.Once
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		if block.Load() && snapshot.uri == uri && snapshot.version == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return diagnosticsForSnapshot(snapshot)
	}
	tx.open(t, uri, "SELECT ID FROM T WHERE V = NULL;", 1)
	tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	block.Store(true)
	tx.server.queueDiagnosticDocument(uri)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("version 1 analysis did not enter the gate")
	}

	// The version-N+1 edit request and an unrelated editor request must return
	// while N's analyzer is blocked. The queued N+1 diagnostic is not expected
	// to publish until the single diagnostics worker is released.
	request := func(method string, params any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(tx.ctx, 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tx.clientConn.Call(ctx, method, params, nil) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s while diagnostics are gated: %v", method, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s did not return while version 1 diagnostics were gated", method)
		}
	}
	request("textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "SELECT 1;"}},
	})
	request("textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: otherURI, LanguageID: "sql", Version: 1, Text: "SELECT 2;"},
	})
	block.Store(false)
	releaseGate()
	latest := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	if len(latest.Diagnostics) != 0 || string(latest.rawDiagnostics) != "[]" || latest.Version == nil || *latest.Version != 2 {
		t.Fatalf("latest version diagnostics = %+v (%s), want []", latest.Diagnostics, latest.rawDiagnostics)
	}
	other := tx.client.next(t, otherURI, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if other.Version == nil || *other.Version != 1 {
		t.Fatalf("unrelated document publication = %+v, want version 1", other)
	}
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && notification.Version != nil && *notification.Version == 1 {
			t.Fatalf("stale version 1 notification published after version 2, regardless of diagnostics payload: %+v", notification)
		}
	}
}

func TestDiagnosticExpansionFailedMetadataRefreshKeepsOnlyIndependentFindings(t *testing.T) {
	const uri = "file:///diagnostic-expansion-failed-metadata.sql"
	text := "SELECT MISSING_COL FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(initial.Diagnostics) != 1 || diagnosticCode(initial.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("no-catalog diagnostics = %+v, want only independent null-comparison warning", initial.Diagnostics)
	}

	fail := func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
		return database.MetadataPatch{}, errors.New("simulated metadata refresh failure")
	}
	repo := readinessPlanRepository{
		DBRepository: &database.MockDBRepository{},
		plan: database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
			{Kind: database.MetadataRelations, Run: fail},
			{Kind: database.MetadataViews, Run: fail},
			{Kind: database.MetadataProcedures, Run: fail},
			{Kind: database.MetadataColumnsCurrent, Run: fail},
		}},
	}
	loadMetadataForTest(t, tx.server, repo)
	status := tx.server.metadata.Snapshot().Status
	for _, kind := range []database.MetadataKind{database.MetadataRelations, database.MetadataViews, database.MetadataProcedures, database.MetadataColumnsCurrent} {
		if status[kind].State != database.MetadataFailed {
			t.Fatalf("metadata category %s state = %s, want failed", kind, status[kind].State)
		}
	}
	refreshed := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(refreshed.Diagnostics) != 1 || diagnosticCode(refreshed.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("failed-refresh diagnostics = %+v, want only the metadata-independent finding", refreshed.Diagnostics)
	}
	if string(refreshed.rawDiagnostics) == "null" || string(refreshed.rawDiagnostics) == "" || refreshed.rawDiagnostics[0] != '[' {
		t.Fatalf("failed-refresh wire diagnostics = %s, want JSON array", refreshed.rawDiagnostics)
	}
}

// --- metadata categories becoming ready in a different order: a Task 4
// unknown-column finding must appear only once its category is ready ---

func TestDiagnosticsMetadataCategoryOrderingGatesUnknownColumnFinding(t *testing.T) {
	const uri = "file:///metadata-ordering.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, "SELECT MISSING_COL FROM T;", 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(initial.Diagnostics) != 0 {
		t.Fatalf("diagnostics before any metadata = %+v, want none", initial.Diagnostics)
	}

	columnsGate := make(chan struct{})
	var columnsGateClosed atomic.Bool
	closeColumnsGate := func() {
		if columnsGateClosed.CompareAndSwap(false, true) {
			close(columnsGate)
		}
	}
	t.Cleanup(closeColumnsGate)

	plan := readinessPlanRepository{
		DBRepository: &database.MockDBRepository{},
		plan: database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
			{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{SchemaTables: map[string][]string{"": {"T"}}}}, nil
			}},
			{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{}}}}, nil
			}},
			{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{}}}}, nil
			}},
			{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				<-columnsGate
				return database.MetadataPatch{Cache: &database.DBCache{ColumnsWithParent: map[string][]*database.ColumnDesc{
					"\tT": {{ColumnBase: database.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"}},
				}}}, nil
			}},
		}},
	}

	tx.server.diagnosticsPublishMu.Lock()
	tx.server.stateMu.Lock()
	tx.server.connGeneration++
	generation := tx.server.connGeneration
	tx.server.metadata.Reset(uint64(generation))
	tx.server.stateMu.Unlock()
	tx.server.diagnosticsPublishMu.Unlock()
	load, err := tx.server.metadata.Start(tx.ctx, uint64(generation), plan)
	if err != nil {
		t.Fatalf("start metadata: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		snapshot := tx.server.metadata.Snapshot()
		if snapshot != nil &&
			snapshot.Status[database.MetadataRelations].State == database.MetadataReady &&
			snapshot.Status[database.MetadataViews].State == database.MetadataReady &&
			snapshot.Status[database.MetadataProcedures].State == database.MetadataReady {
			break
		}
		select {
		case <-deadline:
			t.Fatal("relations/views/procedures did not settle while columns remained gated")
		default:
			runtime.Gosched()
		}
	}
	tx.server.republishOpenDiagnostics(tx.ctx)
	beforeColumns := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(beforeColumns.Diagnostics) != 0 {
		t.Fatalf("diagnostics before columns are ready = %+v, want none: unknown-column is not yet provable", beforeColumns.Diagnostics)
	}

	closeColumnsGate()
	select {
	case <-load.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata load did not settle after releasing the columns gate")
	}
	tx.server.republishOpenDiagnostics(tx.ctx)
	afterColumns := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(afterColumns.Diagnostics[0]) != "interbase-unknown-column" {
		t.Fatalf("diagnostics = %+v, want interbase-unknown-column once relations/views/procedures/columns are all ready", afterColumns.Diagnostics)
	}
}

// --- a local-type-only finding (Task 10's interbase-invalid-assignment on a
// procedure local, which needs no catalog metadata at all: a DECLARE
// VARIABLE's own type is known from the document text alone) must appear
// immediately and survive metadata staying only partially loaded, unlike
// interbase-unknown-column above ---

func TestDiagnosticsInvalidAssignmentProcedureLocalSurvivesPartialMetadata(t *testing.T) {
	const uri = "file:///invalid-assignment-partial-metadata.sql"
	// V is used (assigned) but never read, so interbase-unused also fires
	// alongside interbase-invalid-assignment -- both need no catalog lookup.
	text := "CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN V = 99999; END;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && hasDiagnosticCode(n.Diagnostics, "interbase-invalid-assignment")
	})
	if !hasDiagnosticCode(initial.Diagnostics, "interbase-invalid-assignment") {
		t.Fatalf("initial diagnostics = %+v, want interbase-invalid-assignment before any metadata category is ready", initial.Diagnostics)
	}

	columnsGate := make(chan struct{})
	var columnsGateClosed atomic.Bool
	closeColumnsGate := func() {
		if columnsGateClosed.CompareAndSwap(false, true) {
			close(columnsGate)
		}
	}
	t.Cleanup(closeColumnsGate)

	plan := readinessPlanRepository{
		DBRepository: &database.MockDBRepository{},
		plan: database.MetadataPlan{Parallelism: 2, Jobs: []database.MetadataJob{
			{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{SchemaTables: map[string][]string{}}}, nil
			}},
			{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				<-columnsGate
				return database.MetadataPatch{Cache: &database.DBCache{}}, nil
			}},
		}},
	}

	tx.server.diagnosticsPublishMu.Lock()
	tx.server.stateMu.Lock()
	tx.server.connGeneration++
	generation := tx.server.connGeneration
	tx.server.metadata.Reset(uint64(generation))
	tx.server.stateMu.Unlock()
	tx.server.diagnosticsPublishMu.Unlock()
	if _, err := tx.server.metadata.Start(tx.ctx, uint64(generation), plan); err != nil {
		t.Fatalf("start metadata: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		snapshot := tx.server.metadata.Snapshot()
		if snapshot != nil && snapshot.Status[database.MetadataRelations].State == database.MetadataReady {
			break
		}
		select {
		case <-deadline:
			t.Fatal("relations did not settle while columns remained gated")
		default:
			runtime.Gosched()
		}
	}

	// Columns are still gated (not ready) here, yet this finding needs no
	// catalog lookup at all -- it must keep appearing, not disappear behind
	// a "wait for metadata" fence that only interbase-unknown-column needs.
	tx.server.republishOpenDiagnostics(tx.ctx)
	stillFound := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && hasDiagnosticCode(n.Diagnostics, "interbase-invalid-assignment")
	})
	if !hasDiagnosticCode(stillFound.Diagnostics, "interbase-invalid-assignment") {
		t.Fatalf("diagnostics while columns metadata is still gated = %+v, want interbase-invalid-assignment", stillFound.Diagnostics)
	}
	closeColumnsGate()
}

func hasDiagnosticCode(diagnostics []lsp.Diagnostic, code string) bool {
	for _, d := range diagnostics {
		if diagnosticCode(d) == code {
			return true
		}
	}
	return false
}

// --- a rule-policy change requeues open documents and changes published severity ---

func TestDiagnosticsConfigChangeRequeuesAndChangesPublishedSeverity(t *testing.T) {
	const uri = "file:///policy-severity-change.sql"
	text := "SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(initial.Diagnostics[0]) != "interbase-null-comparison" || initial.Diagnostics[0].Severity != 2 {
		t.Fatalf("initial diagnostics = %+v, want a single warning-severity interbase-null-comparison finding", initial.Diagnostics)
	}

	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	raw := marshalConfigChangeParams(t, cfg)
	if _, err := tx.server.handleWorkspaceDidChangeConfiguration(tx.ctx, tx.serverConn, &jsonrpc2.Request{Params: &raw}); err != nil {
		t.Fatal(err)
	}

	updated := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1 && n.Diagnostics[0].Severity == 1
	})
	if diagnosticCode(updated.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics after policy change = %+v, want interbase-null-comparison at error severity", updated.Diagnostics)
	}
}

// --- a slow analysis started under an old policy must never overwrite a
// publication made under a newer policy revision ---

func TestDiagnosticsSlowOldPolicyAnalysisLosesToNewPolicy(t *testing.T) {
	const uri = "file:///policy-race.sql"
	text := "SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if initial.Diagnostics[0].Severity != 2 {
		t.Fatalf("initial severity = %d, want 2 (warning, the registry default)", initial.Diagnostics[0].Severity)
	}

	// Model a computation that started under the current (old) policy:
	// capture its snapshot and finished output now, but only attempt
	// publication after the policy has already advanced.
	oldSnapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not snapshot open document")
	}
	oldDiagnostics := diagnosticsForSnapshot(oldSnapshot)

	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	raw := marshalConfigChangeParams(t, cfg)
	if _, err := tx.server.handleWorkspaceDidChangeConfiguration(tx.ctx, tx.serverConn, &jsonrpc2.Request{Params: &raw}); err != nil {
		t.Fatal(err)
	}
	updated := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1 && n.Diagnostics[0].Severity == 1
	})
	if diagnosticCode(updated.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics after policy change = %+v, want interbase-null-comparison at error severity", updated.Diagnostics)
	}

	if tx.server.diagnosticsSnapshotCurrent(oldSnapshot) {
		t.Fatal("pre-policy-change snapshot remained current after the policy revision advanced")
	}
	tx.server.publishDiagnosticsSnapshot(tx.ctx, tx.serverConn, oldSnapshot, oldDiagnostics)
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && len(notification.Diagnostics) == 1 && notification.Diagnostics[0].Severity == 2 {
			t.Fatalf("stale pre-policy-change (warning severity) diagnostics were published after the policy changed to error: %+v", notification)
		}
	}
}

// --- diagnostics never trigger actual database I/O ---

func TestDiagnosticsNeverTriggerDatabaseIO(t *testing.T) {
	const uri = "file:///no-io.sql"
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)

	var locked atomic.Bool
	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Schema: "TEST", Table: "DST", Name: "VALUE"}, Type: "VARCHAR(20)"},
		{ColumnBase: database.ColumnBase{Schema: "TEST", Table: "SRC", Name: "VALUE"}, Type: "VARCHAR(40)"},
	}
	guard := func(name string) {
		if locked.Load() {
			panic("diagnostics triggered unexpected database I/O via " + name)
		}
	}
	repo := &database.MockDBRepository{
		MockDatabase: func(context.Context) (string, error) {
			guard("CurrentDatabase")
			return "TEST", nil
		},
		MockDatabases: func(context.Context) ([]string, error) {
			guard("Databases")
			return []string{"TEST"}, nil
		},
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			guard("SchemaTables")
			return map[string][]string{"TEST": {"DST", "SRC"}}, nil
		},
		MockDescribeDatabaseTable: func(context.Context) ([]*database.ColumnDesc, error) {
			guard("DescribeDatabaseTable")
			return columns, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			guard("DescribeDatabaseTableBySchema")
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			guard("DescribeForeignKeysBySchema")
			return nil, nil
		},
		MockExec: func(context.Context, string) (sql.Result, error) {
			guard("Exec")
			return nil, nil
		},
		MockQuery: func(context.Context, string) (*sql.Rows, error) {
			guard("Query")
			return nil, nil
		},
	}
	loadMetadataForTest(t, tx.server, repo)
	locked.Store(true)

	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 2
	})
	if len(initial.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v, want unused hint and truncation warning", initial.Diagnostics)
	}

	fixed := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = UNUSED; INSERT INTO DST (VALUE) VALUES ('x'); END"
	tx.change(t, uri, fixed, 2)
	cleared := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 0
	})
	if len(cleared.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v, want none after the fix; a leaked repository call would have panicked instead", cleared.Diagnostics)
	}
}

// --- live settings changes via didChangeConfiguration must be validated;
// an invalid rule policy must never be silently applied ---

func TestDiagnosticsConfigChangeRejectsUnknownRuleCodeOverRealRPC(t *testing.T) {
	const uri = "file:///policy-reject-unknown-code.sql"
	text := "SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if initial.Diagnostics[0].Severity != 2 {
		t.Fatalf("initial severity = %d, want 2 (warning, the registry default)", initial.Diagnostics[0].Severity)
	}

	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{
		// A realistic typo: "comparision" instead of "comparison".
		"interbase-null-comparision": "off",
	}}}
	err := tx.clientConn.Call(tx.ctx, "workspace/didChangeConfiguration", didChangeConfigurationParams(cfg), nil)
	if err == nil {
		t.Fatal("workspace/didChangeConfiguration with an unknown rule code returned no error, want a rejection")
	}

	tx.server.stateMu.RLock()
	wsCfg := tx.server.WSCfg
	tx.server.stateMu.RUnlock()
	if wsCfg != nil {
		t.Fatalf("WSCfg = %+v, want nil (unchanged): a rejected configuration must never be applied", wsCfg)
	}

	// The previous (default) policy must still be in effect: the rule the
	// bad config tried (and failed) to turn off must still fire.
	tx.change(t, uri, text, 2)
	stillFiring := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(stillFiring.Diagnostics[0]) != "interbase-null-comparison" || stillFiring.Diagnostics[0].Severity != 2 {
		t.Fatalf("diagnostics after a rejected config change = %+v, want the unaffected default policy still in effect", stillFiring.Diagnostics)
	}
}

func TestDiagnosticsConfigChangeRejectsUnknownLevelOverRealRPC(t *testing.T) {
	const uri = "file:///policy-reject-unknown-level.sql"
	text := "SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if initial.Diagnostics[0].Severity != 2 {
		t.Fatalf("initial severity = %d, want 2 (warning, the registry default)", initial.Diagnostics[0].Severity)
	}

	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{
		// Wrong case: registered levels are lowercase ("error"), not "Error".
		"interbase-null-comparison": "Error",
	}}}
	err := tx.clientConn.Call(tx.ctx, "workspace/didChangeConfiguration", didChangeConfigurationParams(cfg), nil)
	if err == nil {
		t.Fatal("workspace/didChangeConfiguration with an unknown level returned no error, want a rejection")
	}

	tx.server.stateMu.RLock()
	wsCfg := tx.server.WSCfg
	tx.server.stateMu.RUnlock()
	if wsCfg != nil {
		t.Fatalf("WSCfg = %+v, want nil (unchanged): a rejected configuration must never be applied", wsCfg)
	}

	tx.change(t, uri, text, 2)
	stillDefault := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(stillDefault.Diagnostics[0]) != "interbase-null-comparison" || stillDefault.Diagnostics[0].Severity != 2 {
		t.Fatalf("diagnostics after a rejected config change = %+v, want the unaffected default policy (severity 2) still in effect, not a severity-less finding", stillDefault.Diagnostics)
	}
}

// --- a genuinely concurrent in-flight diagnostic computation started under
// an old policy revision must never publish its result after a newer policy
// revision has already taken effect ---

func TestDiagnosticsConcurrentInFlightOldPolicyComputationNeverPublishes(t *testing.T) {
	const uri = "file:///concurrent-policy-race.sql"
	text := "SELECT ID FROM T WHERE V = NULL;"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(doRelease)
	var block atomic.Bool

	// diagnosticAnalyzer is invoked directly on the single background
	// diagnostics-worker goroutine (runDiagnosticSignals ->
	// publishDocumentDiagnostics). Blocking inside it here models a real
	// in-flight computation that a concurrent didChangeConfiguration RPC
	// (handled on its own jsonrpc2 dispatch goroutine) can race against.
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		if block.Load() {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		return diagnosticsForSnapshot(snapshot)
	}

	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if initial.Diagnostics[0].Severity != 2 {
		t.Fatalf("initial severity = %d, want 2 (warning, the registry default)", initial.Diagnostics[0].Severity)
	}

	// Arm the hook, then requeue this document so the single background
	// worker picks it up and blocks mid-computation, still under the OLD
	// policy revision.
	block.Store(true)
	tx.server.queueDiagnosticDocument(uri)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight computation did not start")
	}

	// While that computation is genuinely blocked on the background worker
	// goroutine, send a real didChangeConfiguration over JSON-RPC (a
	// separate goroutine) changing the rule to error severity. The RPC call
	// completes without waiting for the busy worker: queueAllDiagnostics
	// only signals a buffered channel.
	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	if err := tx.clientConn.Call(tx.ctx, "workspace/didChangeConfiguration", didChangeConfigurationParams(cfg), nil); err != nil {
		t.Fatal(err)
	}

	// Let any FUTURE analyzer calls (the requeued re-evaluation under the
	// new policy) run normally, then release the stale in-flight call.
	block.Store(false)
	doRelease()

	updated := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1 && n.Diagnostics[0].Severity == 1
	})
	if diagnosticCode(updated.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics after policy change = %+v, want interbase-null-comparison at error severity", updated.Diagnostics)
	}

	// Prove the stale in-flight (old-policy, severity-2) computation, which
	// returned only after being released above, never reached the client:
	// bar for any straggling notification after everything has settled.
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && len(notification.Diagnostics) == 1 && notification.Diagnostics[0].Severity == 2 {
			t.Fatalf("stale in-flight old-policy computation was published after the policy changed: %+v", notification)
		}
	}
}
