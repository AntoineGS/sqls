package language_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

type fakeChecker struct {
	calls         []string
	results       map[string]*language.StatementError
	defaultResult *language.StatementError
	err           error
	errs          map[string]error
	onCall        func(context.Context, string, int)
}

type generationRaceChecker struct {
	calls        atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
	firstResult  *language.StatementError
}

func (c *generationRaceChecker) CheckStatement(ctx context.Context, _ string) (*language.StatementError, error) {
	if c.calls.Add(1) == 1 {
		close(c.firstStarted)
		select {
		case <-c.releaseFirst:
			return c.firstResult, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

func (f *fakeChecker) CheckStatement(ctx context.Context, sql string) (*language.StatementError, error) {
	f.calls = append(f.calls, sql)
	if f.onCall != nil {
		f.onCall(ctx, sql, len(f.calls))
	}
	if err := f.errs[sql]; err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if result, ok := f.results[sql]; ok {
		return result, nil
	}
	return f.defaultResult, nil
}

func serverRequest(text string) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:server-diagnostics", Text: text},
		Context: language.Context{
			Key:             metadata.Key{Engine: "interbase", URL: "fixture://server-diagnostics"},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
}

func expectedRange(t *testing.T, text string, start, end int) language.Range {
	t.Helper()
	startPosition, err := language.PositionAt(text, start)
	if err != nil {
		t.Fatalf("PositionAt(start=%d): %v", start, err)
	}
	endPosition, err := language.PositionAt(text, end)
	if err != nil {
		t.Fatalf("PositionAt(end=%d): %v", end, err)
	}
	return language.Range{Start: startPosition, End: endPosition}
}

func findDiagnostic(diagnostics []language.Diagnostic, code string) (language.Diagnostic, bool) {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return diagnostic, true
		}
	}
	return language.Diagnostic{}, false
}

func TestServerDiagnosticsMapsRoutineServerLocationToDocument(t *testing.T) {
	text := "SET TERM ^ ;\n" +
		"CREATE PROCEDURE P AS\n" +
		"BEGIN\n" +
		"  X = 1;\n" +
		"  X = 2;\n" +
		"  X = 3;\n" +
		"  X = 4;\n" +
		"  _SELECT 1 FROM T;\n" +
		"END^\n" +
		"SET TERM ; ^"
	request := serverRequest(text)
	checker := &fakeChecker{defaultResult: &language.StatementError{
		SQLCode: -104,
		Message: "Dynamic SQL Error\nSQL error code = -104\nToken unknown - line 7, char 3\n_SELECT",
		Line:    7,
		Column:  3,
		Token:   "_SELECT",
	}}
	result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	if len(result.Value) != 1 {
		t.Fatalf("ServerDiagnostics() returned %d findings, want 1: %+v", len(result.Value), result.Value)
	}
	diagnostic := result.Value[0]
	start := strings.Index(text, "_SELECT")
	wantRange := expectedRange(t, text, start, start+len("_SELECT"))
	if diagnostic.Code != "interbase-syntax" || diagnostic.Range != wantRange || diagnostic.Severity != 1 ||
		diagnostic.Source != "sqls" || diagnostic.Message != "Token unknown: _SELECT" {
		t.Fatalf("server diagnostic = %+v, want code interbase-syntax, range %+v, error severity, sqls, and parsed message",
			diagnostic, wantRange)
	}
}

func TestServerDiagnosticsAcceptsByteAndCharacterColumnsAroundUnicode(t *testing.T) {
	sql := "SELECT 'é' FROM A WHERE _X = 1"
	marker := strings.Index(sql, "_X")
	columns := []struct {
		name   string
		column int
	}{
		{name: "byte column", column: marker + 1},
		{name: "character column", column: utf8.RuneCountInString(sql[:marker]) + 1},
	}
	for _, test := range columns {
		t.Run(test.name, func(t *testing.T) {
			request := serverRequest(sql)
			checker := &fakeChecker{defaultResult: &language.StatementError{
				SQLCode: -104, Message: "Token unknown", Line: 1, Column: test.column, Token: "_X",
			}}
			result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
			if err != nil {
				t.Fatalf("ServerDiagnostics() error = %v", err)
			}
			if len(result.Value) != 1 || result.Value[0].Range != expectedRange(t, sql, marker, marker+2) {
				t.Fatalf("ServerDiagnostics() = %+v, want _X range", result.Value)
			}
		})
	}
}

func TestServerDiagnosticsLocatesUnexpectedEndUnknownNameAndFallback(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		serverErr language.StatementError
		wantStart int
		wantEnd   int
	}{
		{
			name:      "unexpected end points at last lexeme",
			sql:       "SELECT * FROM T WHERE",
			serverErr: language.StatementError{SQLCode: -104, Message: "Unexpected end of command"},
			wantStart: len("SELECT * FROM T "),
			wantEnd:   len("SELECT * FROM T WHERE"),
		},
		{
			name:      "unknown table points at object name",
			sql:       "SELECT * FROM NO_SUCH_TABLE",
			serverErr: language.StatementError{SQLCode: -204, Message: "Table unknown", Token: "NO_SUCH_TABLE"},
			wantStart: len("SELECT * FROM "),
			wantEnd:   len("SELECT * FROM NO_SUCH_TABLE"),
		},
		{
			name:      "quoted name compares exactly",
			sql:       `SELECT * FROM "MiXeD"`,
			serverErr: language.StatementError{SQLCode: -204, Message: "Table unknown", Token: "MiXeD"},
			wantStart: len("SELECT * FROM "),
			wantEnd:   len(`SELECT * FROM "MiXeD"`),
		},
		{
			name:      "unknown position falls back to first line",
			sql:       "SELECT UNKNOWN\nFROM T",
			serverErr: language.StatementError{SQLCode: -204, Message: "Access denied"},
			wantStart: 0,
			wantEnd:   len("SELECT UNKNOWN"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := serverRequest(test.sql)
			checker := &fakeChecker{defaultResult: &test.serverErr}
			result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
			if err != nil {
				t.Fatalf("ServerDiagnostics() error = %v", err)
			}
			if len(result.Value) != 1 {
				t.Fatalf("ServerDiagnostics() returned %d findings, want 1: %+v", len(result.Value), result.Value)
			}
			wantRange := expectedRange(t, test.sql, test.wantStart, test.wantEnd)
			if result.Value[0].Range != wantRange {
				t.Fatalf("diagnostic range = %+v, want %+v", result.Value[0].Range, wantRange)
			}
		})
	}
}

func TestServerDiagnosticsPadsOnlyTopLevelNamedMarkers(t *testing.T) {
	service := language.NewService()
	topLevel := "SELECT * FROM T WHERE ID = :ORDERID"
	checker := &fakeChecker{}
	if _, err := service.ServerDiagnostics(context.Background(), serverRequest(topLevel), checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("ServerDiagnostics(top-level) error = %v", err)
	}
	if len(checker.calls) != 1 {
		t.Fatalf("checker calls = %d, want 1", len(checker.calls))
	}
	if got := checker.calls[0]; len(got) != len(topLevel) || strings.Contains(got, ":ORDERID") || !strings.Contains(got, "?") {
		t.Fatalf("checker received %q, want a same-length padded named marker", got)
	}

	routine := "SET TERM ^ ;\nCREATE PROCEDURE P AS\nBEGIN\n  X = :VAR;\nEND^\nSET TERM ; ^"
	routineChecker := &fakeChecker{}
	if _, err := service.ServerDiagnostics(context.Background(), serverRequest(routine), routineChecker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("ServerDiagnostics(routine) error = %v", err)
	}
	if len(routineChecker.calls) != 1 || !strings.Contains(routineChecker.calls[0], ":VAR") {
		t.Fatalf("routine checker calls = %q, want one unmodified PSQL statement", routineChecker.calls)
	}
}

func TestServerDiagnosticsCachesOutcomesByStatementText(t *testing.T) {
	service := language.NewService()
	checker := &fakeChecker{}
	request := serverRequest("SELECT 1; SELECT 2;")
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("first ServerDiagnostics() error = %v", err)
	}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("second ServerDiagnostics() error = %v", err)
	}
	if len(checker.calls) != 2 {
		t.Fatalf("unchanged text caused %d checker calls, want 2 total", len(checker.calls))
	}

	request.Document.Text = "SELECT 1; SELECT 3;"
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("edited ServerDiagnostics() error = %v", err)
	}
	if len(checker.calls) != 3 || checker.calls[2] != "SELECT 3" {
		t.Fatalf("checker calls after one-statement edit = %q, want only SELECT 3 checked again", checker.calls)
	}
}

func TestServerDiagnosticsDoesNotShareCacheAcrossDialects(t *testing.T) {
	service := language.NewService()
	request := serverRequest(`SELECT "VALUE" FROM RDB$DATABASE`)
	request.Context.Variant.Variant = dialect.SQLVariantInterBase3
	dialect3Checker := &fakeChecker{defaultResult: &language.StatementError{
		SQLCode: -104, Message: "Token unknown\nVALUE", Token: "VALUE",
	}}
	dialect3, err := service.ServerDiagnostics(context.Background(), request, dialect3Checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("dialect-3 ServerDiagnostics() error = %v", err)
	}
	if len(dialect3.Value) != 1 || len(dialect3Checker.calls) != 1 {
		t.Fatalf("dialect-3 diagnostics=%+v checker calls=%d, want one rejection and one prepare", dialect3.Value, len(dialect3Checker.calls))
	}

	request.Context.Variant.Variant = dialect.SQLVariantInterBase1
	dialect1Checker := &fakeChecker{}
	dialect1, err := service.ServerDiagnostics(context.Background(), request, dialect1Checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("dialect-1 ServerDiagnostics() error = %v", err)
	}
	if len(dialect1.Value) != 0 || len(dialect1Checker.calls) != 1 {
		t.Fatalf("dialect-1 diagnostics=%+v checker calls=%d, want accepted outcome from a distinct prepare", dialect1.Value, len(dialect1Checker.calls))
	}
}

func TestServerDiagnosticsDoesNotShareCacheAcrossPartitions(t *testing.T) {
	service := language.NewService()
	request := serverRequest("SELECT * FROM PRIVATE_TABLE")
	partitionA := language.ServerDiagnosticsOptions{Partition: "scope-a"}
	partitionB := language.ServerDiagnosticsOptions{Partition: "scope-b"}
	checkerA := &fakeChecker{defaultResult: &language.StatementError{
		SQLCode: -204, Message: "Table unknown\nPRIVATE_TABLE", Token: "PRIVATE_TABLE",
	}}
	if result, err := service.ServerDiagnostics(context.Background(), request, checkerA, partitionA); err != nil || len(result.Value) != 1 {
		t.Fatalf("partition-a ServerDiagnostics() = (%+v, %v), want its server rejection", result.Value, err)
	}

	cachedB, err := service.ServerDiagnostics(context.Background(), request, nil,
		language.ServerDiagnosticsOptions{CachedOnly: true, Partition: partitionB.Partition})
	if err != nil {
		t.Fatalf("partition-b CachedOnly ServerDiagnostics() error = %v", err)
	}
	if len(cachedB.Value) != 0 {
		t.Fatalf("partition-b reused partition-a diagnostics: %+v", cachedB.Value)
	}

	checkerB := &fakeChecker{}
	fullB, err := service.ServerDiagnostics(context.Background(), request, checkerB, partitionB)
	if err != nil {
		t.Fatalf("partition-b ServerDiagnostics() error = %v", err)
	}
	if len(fullB.Value) != 0 || len(checkerB.calls) != 1 {
		t.Fatalf("partition-b diagnostics=%+v checker calls=%d, want accepted result from one independent prepare", fullB.Value, len(checkerB.calls))
	}

	cachedA, err := service.ServerDiagnostics(context.Background(), request, nil,
		language.ServerDiagnosticsOptions{CachedOnly: true, Partition: partitionA.Partition})
	if err != nil || len(cachedA.Value) != 1 {
		t.Fatalf("partition-a CachedOnly ServerDiagnostics() = (%+v, %v), want its cached rejection", cachedA.Value, err)
	}
}

func TestServerDiagnosticsEvictsLeastRecentlyUsedCachedStatement(t *testing.T) {
	var statements []string
	for i := 0; i < 2049; i++ {
		statements = append(statements, fmt.Sprintf("SELECT %d", i))
	}
	request := serverRequest(strings.Join(statements, "; "))
	service := language.NewService()
	checker := &fakeChecker{}
	for run := 0; run < 11; run++ {
		if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
			t.Fatalf("ServerDiagnostics() run %d error = %v", run+1, err)
		}
	}
	if len(checker.calls) != 2049 {
		t.Fatalf("initial checker calls = %d, want all 2049 unique statements", len(checker.calls))
	}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("ServerDiagnostics() after LRU fill error = %v", err)
	}
	if len(checker.calls) != 2249 {
		t.Fatalf("checker calls after LRU eviction = %d, want 2249 (200 evicted statements re-prepared)", len(checker.calls))
	}
}

func TestServerDiagnosticsCachedOnlyReturnsCachedFindingsWithoutIO(t *testing.T) {
	service := language.NewService()
	text := "SELECT _BROKEN; SELECT 2;"
	request := serverRequest(text)
	checker := &fakeChecker{results: map[string]*language.StatementError{
		"SELECT _BROKEN": {SQLCode: -104, Message: "Token unknown\n_BROKEN", Token: "_BROKEN"},
	}}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("full ServerDiagnostics() error = %v", err)
	}
	request.Document.Text = "SELECT _BROKEN; SELECT 3;"
	result, err := service.ServerDiagnostics(context.Background(), request, nil, language.ServerDiagnosticsOptions{CachedOnly: true})
	if err != nil {
		t.Fatalf("CachedOnly ServerDiagnostics() error = %v", err)
	}
	if len(result.Value) != 1 || result.Value[0].Code != "interbase-syntax" {
		t.Fatalf("CachedOnly diagnostics = %+v, want only cached first-statement finding", result.Value)
	}
	if len(checker.calls) != 2 {
		t.Fatalf("CachedOnly performed I/O: checker calls = %d, want 2 from initial full run", len(checker.calls))
	}
}

func TestServerDiagnosticsInvalidatesCacheOnMetadataAndIncarnationChanges(t *testing.T) {
	service := language.NewService()
	checker := &fakeChecker{}
	request := serverRequest("SELECT 1")
	request.Context.Snapshot = &catalog.MetadataSnapshot{Generation: 3, Revision: 5}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("initial ServerDiagnostics() error = %v", err)
	}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("cached ServerDiagnostics() error = %v", err)
	}
	if len(checker.calls) != 1 {
		t.Fatalf("stable identity caused %d checker calls, want 1", len(checker.calls))
	}
	for _, change := range []struct {
		name   string
		update func(*language.Request)
	}{
		{name: "generation", update: func(req *language.Request) { req.Context.Snapshot.Generation++ }},
		{name: "revision", update: func(req *language.Request) { req.Context.Snapshot.Revision++ }},
		{name: "incarnation", update: func(req *language.Request) { req.Context.Incarnation++ }},
	} {
		t.Run(change.name, func(t *testing.T) {
			before := len(checker.calls)
			change.update(&request)
			if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
				t.Fatalf("ServerDiagnostics() error = %v", err)
			}
			if len(checker.calls) != before+1 {
				t.Fatalf("identity change caused %d checker calls, want one recheck", len(checker.calls)-before)
			}
		})
	}
	if len(checker.calls) != 4 {
		t.Fatalf("checker calls after generation, revision and incarnation changes = %d, want 4", len(checker.calls))
	}
}

func TestServerDiagnosticsDoesNotCacheStaleGenerationResult(t *testing.T) {
	service := language.NewService()
	request := serverRequest("SELECT _STALE")
	request.Context.Snapshot = &catalog.MetadataSnapshot{Generation: 1, Revision: 1}
	newerRequest := request
	newerRequest.Context.Snapshot = &catalog.MetadataSnapshot{Generation: 2, Revision: 1}
	checker := &generationRaceChecker{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		firstResult:  &language.StatementError{SQLCode: -104, Message: "Token unknown\n_STALE", Token: "_STALE"},
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(checker.releaseFirst) }) }
	defer release()

	staleDone := make(chan error, 1)
	go func() {
		result, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
		if err == nil && len(result.Value) != 1 {
			err = fmt.Errorf("generation-1 run returned %d findings, want its server rejection", len(result.Value))
		}
		staleDone <- err
	}()
	select {
	case <-checker.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("generation-1 checker did not start")
	}

	newerResult, err := service.ServerDiagnostics(context.Background(), newerRequest, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		release()
		t.Fatalf("generation-2 ServerDiagnostics() error = %v", err)
	}
	if len(newerResult.Value) != 0 {
		release()
		t.Fatalf("generation-2 accepted statement produced findings: %+v", newerResult.Value)
	}
	release()
	select {
	case err := <-staleDone:
		if err != nil {
			t.Fatalf("generation-1 ServerDiagnostics() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("generation-1 checker did not finish after release")
	}

	cached, err := service.ServerDiagnostics(context.Background(), newerRequest, nil, language.ServerDiagnosticsOptions{CachedOnly: true})
	if err != nil {
		t.Fatalf("generation-2 CachedOnly ServerDiagnostics() error = %v", err)
	}
	if len(cached.Value) != 0 {
		t.Fatalf("generation-2 cached result was poisoned by stale write: %+v", cached.Value)
	}
}

func TestServerDiagnosticsCancellationKeepsEarlierCachedResults(t *testing.T) {
	service := language.NewService()
	request := serverRequest("SELECT _FIRST; SELECT _SECOND;")
	ctx, cancel := context.WithCancel(context.Background())
	checker := &fakeChecker{
		results: map[string]*language.StatementError{
			"SELECT _FIRST":  {SQLCode: -104, Message: "Token unknown\n_FIRST", Token: "_FIRST"},
			"SELECT _SECOND": {SQLCode: -104, Message: "Token unknown\n_SECOND", Token: "_SECOND"},
		},
		onCall: func(_ context.Context, _ string, call int) {
			if call == 2 {
				cancel()
			}
		},
	}
	_, err := service.ServerDiagnostics(ctx, request, checker, language.ServerDiagnosticsOptions{})
	if !errors.Is(err, language.ErrCanceled) {
		t.Fatalf("canceled ServerDiagnostics() error = %v, want ErrCanceled", err)
	}
	cancel()

	result, err := service.ServerDiagnostics(context.Background(), request, nil, language.ServerDiagnosticsOptions{CachedOnly: true})
	if err != nil {
		t.Fatalf("CachedOnly after cancellation error = %v", err)
	}
	if len(result.Value) != 1 || result.Value[0].Message != "Token unknown: _FIRST" {
		t.Fatalf("cached findings after cancellation = %+v, want the first statement only", result.Value)
	}
	if len(checker.calls) != 2 {
		t.Fatalf("checker calls after canceled run = %d, want 2", len(checker.calls))
	}
}

func TestServerDiagnosticsNonSQLCheckerErrorStopsFurtherPrepares(t *testing.T) {
	service := language.NewService()
	request := serverRequest("SELECT _THIRD")
	checker := &fakeChecker{results: map[string]*language.StatementError{
		"SELECT _THIRD": {SQLCode: -104, Message: "Token unknown\n_THIRD", Token: "_THIRD"},
	}}
	if _, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("warm-cache ServerDiagnostics() error = %v", err)
	}
	request = serverRequest("SELECT _FIRST; SELECT 2; SELECT _THIRD;")
	checker = &fakeChecker{
		results: map[string]*language.StatementError{
			"SELECT _FIRST": {SQLCode: -104, Message: "Token unknown\n_FIRST", Token: "_FIRST"},
		},
		errs: map[string]error{"SELECT 2": errors.New("connection lost")},
	}
	result, err := service.ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() returned non-SQL checker error: %v", err)
	}
	if len(checker.calls) != 2 || checker.calls[1] != "SELECT 2" {
		t.Fatalf("checker calls after connection error = %q, want no prepare of SELECT _THIRD", checker.calls)
	}
	if len(result.Value) != 2 || result.Value[0].Message != "Token unknown: _FIRST" || result.Value[1].Message != "Token unknown: _THIRD" {
		t.Fatalf("findings after connection error = %+v, want first and cached third statement findings", result.Value)
	}
}

func TestServerDiagnosticsCapsActualPreparesAtTwoHundred(t *testing.T) {
	var statements []string
	for i := 0; i < 201; i++ {
		statements = append(statements, fmt.Sprintf("SELECT %d", i))
	}
	checker := &fakeChecker{}
	if _, err := language.NewService().ServerDiagnostics(context.Background(), serverRequest(strings.Join(statements, "; ")), checker, language.ServerDiagnosticsOptions{}); err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	if len(checker.calls) != 200 {
		t.Fatalf("checker calls = %d, want 200", len(checker.calls))
	}
}

func TestServerDiagnosticsAppliesRulePolicy(t *testing.T) {
	request := serverRequest("SELECT _BAD; SELECT * FROM MISSING;")
	request.Policy = language.DiagnosticPolicy{Rules: map[string]string{
		"interbase-syntax":       "off",
		"interbase-server-check": "warning",
	}}
	checker := &fakeChecker{results: map[string]*language.StatementError{
		"SELECT _BAD":           {SQLCode: -104, Message: "Token unknown\n_BAD", Token: "_BAD"},
		"SELECT * FROM MISSING": {SQLCode: -204, Message: "Table unknown\nMISSING", Token: "MISSING"},
	}}
	result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	if len(result.Value) != 1 || result.Value[0].Code != "interbase-server-check" || result.Value[0].Severity != 2 {
		t.Fatalf("policy-controlled diagnostics = %+v, want one warning server-check finding", result.Value)
	}
}

func TestServerDiagnosticsSkipsAllPreparesWhenServerRulesAreOff(t *testing.T) {
	request := serverRequest("SELECT _BAD")
	request.Policy = language.DiagnosticPolicy{Rules: map[string]string{
		"interbase-syntax":       "off",
		"interbase-server-check": "off",
	}}
	checker := &fakeChecker{defaultResult: &language.StatementError{
		SQLCode: -104, Message: "Token unknown\n_BAD", Token: "_BAD",
	}}
	result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	if result.Value == nil || len(result.Value) != 0 || len(checker.calls) != 0 {
		t.Fatalf("disabled server diagnostics = value %#v, checker calls %d; want no findings and no prepare", result.Value, len(checker.calls))
	}
}

func TestServerDiagnosticsDropsFindingsOverlappingStaticDiagnostics(t *testing.T) {
	request := serverRequest("SELECT ID FROM T WHERE ID = NULL;")
	checker := &fakeChecker{defaultResult: &language.StatementError{
		SQLCode: -104, Message: "Token unknown\nNULL", Token: "NULL",
	}}
	result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	static, err := language.NewService().Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	if _, ok := findDiagnostic(static.Value, "interbase-null-comparison"); !ok {
		t.Fatalf("static null-comparison finding missing: %+v", static.Value)
	}
	if len(result.Value) != 0 {
		t.Fatalf("overlapping server finding was not deduplicated: %+v", result.Value)
	}
}

func TestServerDiagnosticsReturnsEmptyForUnsupportedOrMalformedRequests(t *testing.T) {
	tests := []struct {
		name    string
		request language.Request
		checker language.StatementChecker
	}{
		{name: "non-InterBase", request: func() language.Request {
			req := serverRequest("SELECT 1")
			req.Context.Variant.Driver = dialect.DatabaseDriverMySQL
			return req
		}(), checker: &fakeChecker{}},
		{name: "unresolved dialect", request: func() language.Request {
			req := serverRequest("SELECT 1")
			req.Context.DialectResolved = false
			return req
		}(), checker: &fakeChecker{}},
		{name: "nil checker in full mode", request: serverRequest("SELECT 1")},
		{name: "unterminated string", request: serverRequest("SELECT 'unterminated"), checker: &fakeChecker{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := language.NewService().ServerDiagnostics(context.Background(), test.request, test.checker, language.ServerDiagnosticsOptions{})
			if err != nil {
				t.Fatalf("ServerDiagnostics() error = %v", err)
			}
			if result.Value == nil || len(result.Value) != 0 {
				t.Fatalf("ServerDiagnostics() = %#v, want empty non-nil list", result.Value)
			}
			if checker, ok := test.checker.(*fakeChecker); ok && len(checker.calls) != 0 {
				t.Fatalf("unsupported request made %d checker calls", len(checker.calls))
			}
		})
	}
}

func TestServerDiagnosticsWorksWithZeroValueService(t *testing.T) {
	var service language.Service
	checker := &fakeChecker{}
	result, err := service.ServerDiagnostics(context.Background(), serverRequest("SELECT 1"), checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("zero-value Service.ServerDiagnostics() error = %v", err)
	}
	if result.Value == nil || len(checker.calls) != 1 {
		t.Fatalf("zero-value service result=%#v calls=%d, want empty non-nil result and one prepare", result.Value, len(checker.calls))
	}
}

func TestServerDiagnosticsReturnsStampAndSortsServerFindings(t *testing.T) {
	text := "SELECT _LATE; SELECT _EARLY;"
	request := serverRequest(text)
	request.Document.Version = 7
	request.Document.Binding = 4
	request.Context.Incarnation = 2
	request.Context.Snapshot = &catalog.MetadataSnapshot{Generation: 9, Revision: 11}
	checker := &fakeChecker{defaultResult: &language.StatementError{SQLCode: -104, Message: "Token unknown", Token: "_LATE"}}
	checker.results = map[string]*language.StatementError{
		"SELECT _LATE":  {SQLCode: -104, Message: "Token unknown\n_LATE", Token: "_LATE"},
		"SELECT _EARLY": {SQLCode: -104, Message: "Token unknown\n_EARLY", Token: "_EARLY"},
	}
	result, err := language.NewService().ServerDiagnostics(context.Background(), request, checker, language.ServerDiagnosticsOptions{})
	if err != nil {
		t.Fatalf("ServerDiagnostics() error = %v", err)
	}
	if result.Stamp.URI != request.Document.URI || result.Stamp.Version != 7 || result.Stamp.Binding != 4 ||
		result.Stamp.Incarnation != 2 || result.Stamp.Generation != 9 || result.Stamp.Revision != 11 || result.Stamp.Key != request.Context.Key {
		t.Fatalf("result stamp = %+v, want stampFor(request)", result.Stamp)
	}
	if len(result.Value) != 2 || result.Value[0].Message != "Token unknown: _LATE" || result.Value[1].Message != "Token unknown: _EARLY" {
		t.Fatalf("findings = %+v, want source order with parsed messages", result.Value)
	}
}
