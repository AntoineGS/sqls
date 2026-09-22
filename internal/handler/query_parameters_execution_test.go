package handler

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/queryparams"
)

// parameterFixture is a connected server whose repository is the
// stub-query-parameters backend, plus the two calls a parameterized run
// makes: discovery, then execution carrying the identity discovery returned.
type parameterFixture struct {
	tx      *TestContext
	backend *parameterBackend
}

// newParameterFixture opens text on a stub InterBase connection. prepare, when
// non-nil, configures the backend before the connection is established: the
// factory runs at that point, and a repository's method set cannot be changed
// afterwards.
func newParameterFixture(t *testing.T, text string, prepare func(*parameterBackend)) *parameterFixture {
	t.Helper()
	tx := newTestContext()
	tx.setup(t)
	t.Cleanup(tx.tearDown)
	t.Cleanup(tx.server.worker.Stop)

	backend := installParameterBackend(t)
	if prepare != nil {
		prepare(backend)
	}
	tx.addWorkspaceConfig(t, stubQueryParametersConnections("primary", "secondary"))

	// The catalog lands on the worker's asynchronous pass, so a routing
	// assertion issued straight afterwards would race it and fall to the
	// unknown-procedure branch.
	if len(backend.describedProcedures()) > 0 {
		waitForCatalog(t, tx.server.worker)
	}
	tx.textDocumentDidOpen(t, testFileURI, text)
	return &parameterFixture{tx: tx, backend: backend}
}

func withProcedures(b *parameterBackend) { b.setProcedures(testProcedures()) }

func (f *parameterFixture) discover(t *testing.T, rng *lsp.Range) lsp.QueryParameterDiscovery {
	t.Helper()
	var got lsp.QueryParameterDiscovery
	if err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandGetQueryParameters,
		Arguments: []interface{}{testFileURI},
		Range:     rng,
	}, &got); err != nil {
		t.Fatal("conn.Call getQueryParameters:", err)
	}
	if !got.Supported {
		t.Fatalf("discovery reported supported=false: %+v", got)
	}
	return got
}

func (f *parameterFixture) execute(t *testing.T, sub *lsp.QueryParameterSubmission, rng *lsp.Range) (string, error) {
	t.Helper()
	var got string
	err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:         CommandExecuteQuery,
		Arguments:       []interface{}{testFileURI},
		Range:           rng,
		ParameterValues: sub,
	}, &got)
	return got, err
}

// submissionFor builds what a client sends back after prompting: the exact
// identity it was prompted under, plus the entered values.
func submissionFor(d lsp.QueryParameterDiscovery, values ...queryparams.Value) *lsp.QueryParameterSubmission {
	return &lsp.QueryParameterSubmission{
		QueryParameterContext: d.QueryParameterContext,
		Values:                values,
	}
}

func textValue(name, value string) queryparams.Value {
	return queryparams.Value{Name: name, Type: "text", Value: value}
}

// requireNoCalls is the assertion every rejection case shares: a refused
// submission must not reach the database at all, not even for its first
// statement.
func requireNoCalls(t *testing.T, backend *parameterBackend) {
	t.Helper()
	if calls := backend.calls(); len(calls) != 0 {
		t.Fatalf("repository served %#v, want no statement executed", calls)
	}
}

func TestParameterExecutionBindsARepeatedNameOnce(t *testing.T) {
	f := newParameterFixture(t, "SELECT * FROM T WHERE A = :EMPLYID OR B = :emplyid", nil)
	d := f.discover(t, nil)

	got, err := f.execute(t, submissionFor(d, textValue("EMPLYID", "000123")), nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("calls = %#v", calls)
	}
	if !reflect.DeepEqual(calls[0].Args, []any{"000123", "000123"}) {
		t.Fatalf("arguments = %#v", calls[0].Args)
	}
	if strings.Contains(calls[0].SQL, ":EMPLYID") || !strings.Contains(calls[0].SQL, "?") {
		t.Errorf("SQL = %q, want the named markers rewritten to positional ones", calls[0].SQL)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rendered row", got)
	}
}

func TestParameterExecutionRunsTheSelectedStatementOnly(t *testing.T) {
	f := newParameterFixture(t, "SELECT :A FROM T;\nSELECT :B FROM U", nil)
	rng := &lsp.Range{
		Start: lsp.Position{Line: 1, Character: 0},
		End:   lsp.Position{Line: 1, Character: utf16Len("SELECT :B FROM U")},
	}
	d := f.discover(t, rng)
	if len(d.Parameters) != 1 || d.Parameters[0].Key != "B" {
		t.Fatalf("Parameters = %+v, want only the selected statement's parameter", d.Parameters)
	}

	if _, err := f.execute(t, submissionFor(d, textValue("B", "2")), rng); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want only the selected statement", calls)
	}
	if !strings.Contains(calls[0].SQL, "FROM U") {
		t.Errorf("SQL = %q, want the selected statement", calls[0].SQL)
	}
	if !reflect.DeepEqual(calls[0].Args, []any{"2"}) {
		t.Errorf("arguments = %#v", calls[0].Args)
	}
}

func TestParameterExecutionRepeatsCaseFoldedNamesAcrossStatements(t *testing.T) {
	f := newParameterFixture(t, "SELECT :Id FROM T;\nSELECT :ID FROM U", nil)
	d := f.discover(t, nil)
	if len(d.Parameters) != 1 {
		t.Fatalf("Parameters = %+v, want one case-folded parameter", d.Parameters)
	}

	if _, err := f.execute(t, submissionFor(d, textValue("id", "7")), nil); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want both statements executed once", calls)
	}
	for i, call := range calls {
		if !reflect.DeepEqual(call.Args, []any{"7"}) {
			t.Errorf("statement %d arguments = %#v, want the one entered value", i+1, call.Args)
		}
	}
}

func TestParameterExecutionAcceptsCommentsBeforeSelect(t *testing.T) {
	f := newParameterFixture(t, "-- leading note\n/* and a block */\nSELECT :ID FROM T", nil)
	d := f.discover(t, nil)

	if _, err := f.execute(t, submissionFor(d, textValue("ID", "9")), nil); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("calls = %#v, want one read-only statement despite the leading comments", calls)
	}
}

func TestParameterExecutionBindsNullThroughTheBoundPath(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM T", nil)
	d := f.discover(t, nil)

	if _, err := f.execute(t, submissionFor(d, queryparams.Value{Name: "ID", Type: "null"}), nil); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("calls = %#v", calls)
	}
	// A nil value still has a nonzero argument count: NULL must bind, not
	// silently fall back to the unparameterized method.
	if !reflect.DeepEqual(calls[0].Args, []any{nil}) {
		t.Fatalf("arguments = %#v, want one bound nil", calls[0].Args)
	}
}

func TestParameterExecutionKeepsPartialResultsOnAReadOnlyFetchError(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID, fail_fetch FROM T", nil)
	d := f.discover(t, nil)

	got, err := f.execute(t, submissionFor(d, textValue("ID", "1")), nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	if calls := f.backend.calls(); len(calls) != 1 || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("calls = %#v", calls)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rows fetched before the failure", got)
	}
	if !strings.Contains(got, "Fetch failed") {
		t.Errorf("result = %q, want the fetch failure reported", got)
	}
}

func TestParameterExecutionCancellationNoticeStaysAuthoritative(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM GATEME", nil)
	d := f.discover(t, nil)

	gate := f.backend.gate("GATEME")
	requestID := jsonrpc2.ID{Str: "bound-cancel", IsString: true}

	type callResult struct {
		out string
		err error
	}
	done := make(chan callResult, 1)
	go func() {
		var got string
		err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:         CommandExecuteQuery,
			Arguments:       []interface{}{testFileURI},
			ParameterValues: submissionFor(d, textValue("ID", "1")),
		}, &got, jsonrpc2.PickID(requestID))
		done <- callResult{out: got, err: err}
	}()
	gate.waitEntered(t)

	if err := f.tx.conn.Notify(f.tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal("conn.Call executeQuery:", res.err)
		}
		// An untagged build classifies the failure as FailureNone, so the
		// notice degrades to the stopped-waiting wording.
		if !strings.Contains(res.out, "sqls stopped waiting for this statement") {
			t.Errorf("result = %q, want the cancellation notice", res.out)
		}
		if strings.Contains(res.out, "42") {
			t.Errorf("result = %q, want no rows for a cancelled statement", res.out)
		}
		if strings.Contains(res.out, lateCancellationNote) {
			t.Errorf("result = %q, want no late-arrival note beside the cancellation notice", res.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled command never completed")
	}
}

// TestParameterExecutionUsesTheSubmittedSnapshot parks the bound statement and
// rewrites the document underneath it. Meaningful under -race: the accepted
// SQL snapshot is the execution input, and later edits must neither mutate it
// nor race the read that produced it.
func TestParameterExecutionUsesTheSubmittedSnapshot(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM GATEME", nil)
	d := f.discover(t, nil)

	gate := f.backend.gate("GATEME")
	done := make(chan error, 1)
	go func() {
		_, err := f.execute(t, submissionFor(d, textValue("ID", "5")), nil)
		done <- err
	}()
	// A sleep, not gate.waitEntered: the document read happens before the
	// repository call, so receiving the gate's signal would order that read
	// ahead of every didChange below and hide the race entirely.
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 50; i++ {
		params := lsp.DidChangeTextDocumentParams{
			TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: i + 1},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: fmt.Sprintf("SELECT %d;", i)}},
		}
		if err := f.tx.conn.Call(f.tx.ctx, "textDocument/didChange", params, nil); err != nil {
			t.Fatal("conn.Call textDocument/didChange:", err)
		}
	}

	gate.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("conn.Call executeQuery:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight bound command never completed")
	}

	calls := f.backend.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want exactly the submitted statement", calls)
	}
	if !strings.Contains(calls[0].SQL, "GATEME") {
		t.Errorf("SQL = %q, want the SQL snapshot accepted at submission", calls[0].SQL)
	}
	if !reflect.DeepEqual(calls[0].Args, []any{"5"}) {
		t.Errorf("arguments = %#v", calls[0].Args)
	}
}

func TestParameterSubmissionRejectsAnInvalidValueBeforeTheFirstStatement(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []queryparams.Value
	}{
		{
			name:   "a value missing for the last statement",
			values: []queryparams.Value{textValue("A", "1")},
		},
		{
			name: "an invalid value used only by the last statement",
			values: []queryparams.Value{
				textValue("A", "1"),
				{Name: "B", Type: "integer", Value: "not-a-number"},
			},
		},
		{
			name: "a value for a parameter this query does not use",
			values: []queryparams.Value{
				textValue("A", "1"), textValue("B", "2"), textValue("C", "3"),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newParameterFixture(t, "SELECT :A FROM T;\nSELECT :B FROM U", nil)
			d := f.discover(t, nil)

			if _, err := f.execute(t, submissionFor(d, tt.values...), nil); err == nil {
				t.Fatal("executeQuery succeeded, want the whole batch refused")
			}
			requireNoCalls(t, f.backend)
		})
	}
}

func TestParameterSubmissionRejectsAnUnsupportedRepositoryBeforeTheFirstStatement(t *testing.T) {
	// The first statement needs no capability at all; the second does. The
	// batch must be refused before the first one runs.
	f := newParameterFixture(t, "SELECT 1 FROM T;\nSELECT :ID FROM U", func(b *parameterBackend) {
		b.withoutParameterCapabilities()
	})
	d := f.discover(t, nil)

	if _, err := f.execute(t, submissionFor(d, textValue("ID", "1")), nil); err == nil {
		t.Fatal("executeQuery succeeded, want a repository without bound capabilities refused")
	}
	requireNoCalls(t, f.backend)
}

func TestParameterSubmissionRejectsAStaleContext(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*lsp.QueryParameterSubmission)
	}{
		{"an unsupported protocol version", func(s *lsp.QueryParameterSubmission) { s.Version = 2 }},
		{"a missing protocol version", func(s *lsp.QueryParameterSubmission) { s.Version = 0 }},
		{"another connection", func(s *lsp.QueryParameterSubmission) { s.ConnectionKey = "0000" }},
		{"another generation", func(s *lsp.QueryParameterSubmission) { s.ConnectionGeneration++ }},
		{"another query", func(s *lsp.QueryParameterSubmission) { s.QueryKey = "0000" }},
		{"another document", func(s *lsp.QueryParameterSubmission) { s.DocumentKey = "0000" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newParameterFixture(t, "SELECT :ID FROM T", nil)
			d := f.discover(t, nil)

			sub := submissionFor(d, textValue("ID", "1"))
			tt.mutate(sub)
			if _, err := f.execute(t, sub, nil); err == nil {
				t.Fatal("executeQuery succeeded, want a stale submission refused")
			}
			requireNoCalls(t, f.backend)
		})
	}
}

func TestParameterSubmissionRejectsADocumentEditedWhilePrompting(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM T", nil)
	d := f.discover(t, nil)

	// An edit outside the selection is still a document change: whole-document
	// validation deliberately refuses it rather than execute a version the
	// user never saw.
	if err := f.tx.conn.Call(f.tx.ctx, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "SELECT :ID FROM T\n-- a new trailing comment"}},
	}, nil); err != nil {
		t.Fatal("conn.Call textDocument/didChange:", err)
	}

	if _, err := f.execute(t, submissionFor(d, textValue("ID", "1")), nil); err == nil {
		t.Fatal("executeQuery succeeded, want the edited document refused")
	}
	requireNoCalls(t, f.backend)
}

func TestParameterSubmissionRejectsAReconnectedConnection(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM T", nil)
	d := f.discover(t, nil)

	if err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandSwitchConnection,
		Arguments: []interface{}{"2"},
	}, nil); err != nil {
		t.Fatal("conn.Call switchConnections:", err)
	}

	if _, err := f.execute(t, submissionFor(d, textValue("ID", "1")), nil); err == nil {
		t.Fatal("executeQuery succeeded, want a submission prompted on another connection refused")
	}
	requireNoCalls(t, f.backend)
}

func TestParameterExecutionRoutesAProcedureWithOutputThroughQuery(t *testing.T) {
	f := newParameterFixture(t, "EXECUTE PROCEDURE MYPROC(:CODE)", withProcedures)
	d := f.discover(t, nil)

	got, err := f.execute(t, submissionFor(d, textValue("CODE", "AB")), nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteQuery {
		// Never read-only: an implicit procedure query commits its write
		// transaction, so a procedure call is a write even when it returns a
		// row. And never both routes: a mutating procedure must not run twice.
		t.Fatalf("calls = %#v, want exactly one Query call", calls)
	}
	if !reflect.DeepEqual(calls[0].Args, []any{"AB"}) {
		t.Errorf("arguments = %#v", calls[0].Args)
	}
	if !strings.Contains(got, executeProcedureOneRowNote) {
		t.Errorf("result = %q, want the one-row note", got)
	}
}

func TestParameterExecutionRoutesAProcedureWithoutOutputThroughExec(t *testing.T) {
	f := newParameterFixture(t, "EXECUTE PROCEDURE DOWORK(:ID)", withProcedures)
	d := f.discover(t, nil)

	got, err := f.execute(t, submissionFor(d, queryparams.Value{Name: "ID", Type: "integer", Value: "1"}), nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteExec {
		t.Fatalf("calls = %#v, want exactly one Exec call", calls)
	}
	if !reflect.DeepEqual(calls[0].Args, []any{int64(1)}) {
		t.Errorf("arguments = %#v, want the typed integer", calls[0].Args)
	}
	if !strings.Contains(got, "Query OK") {
		t.Errorf("result = %q, want the Exec result line", got)
	}
	if strings.Contains(got, executeProcedureOneRowNote) {
		t.Errorf("result = %q, want no one-row note on the Exec path", got)
	}
}

func TestParameterExecutionUnknownProcedureExplainsTheCacheRefresh(t *testing.T) {
	f := newParameterFixture(t, "EXECUTE PROCEDURE NOSUCHPROC(:ID)", withProcedures)
	d := f.discover(t, nil)

	got, err := f.execute(t, submissionFor(d, textValue("ID", "1")), nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteExec {
		t.Fatalf("calls = %#v, want one Exec attempt and no retry on the other route", calls)
	}
	if !strings.Contains(got, "NOSUCHPROC is not in the catalog cache") {
		t.Errorf("result = %q, want the cache-refresh hint naming the procedure", got)
	}
}

func TestParameterSubmissionRefusesLegacyNamedMarkers(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM T", nil)

	got, err := f.execute(t, nil, nil)
	if err == nil {
		t.Fatalf("executeQuery succeeded (%q), want named markers without values refused", got)
	}
	if !strings.Contains(err.Error(), CommandGetQueryParameters) {
		t.Errorf("error = %v, want it to name the command a client needs", err)
	}
	requireNoCalls(t, f.backend)
}

func TestParameterSubmissionLegacyUnparameterizedQueryStillRuns(t *testing.T) {
	f := newParameterFixture(t, "SELECT 1 FROM T", nil)

	got, err := f.execute(t, nil, nil)
	if err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) != 1 || calls[0].Route != parameterRouteReadOnly {
		t.Fatalf("calls = %#v, want the legacy read-only path", calls)
	}
	if calls[0].Args != nil {
		t.Errorf("arguments = %#v, want the legacy repository method with no bound arguments", calls[0].Args)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rendered row", got)
	}
}

func TestParameterSubmissionLegacyPSQLWithVariableMarkersStillRuns(t *testing.T) {
	// A PSQL body's ":V" is a local variable reference, never a prompted
	// input. The legacy guard must not claim it as one, and this batch must
	// keep the unparameterized path it has always taken.
	const body = "EXECUTE BLOCK RETURNS (N INTEGER) AS DECLARE V INTEGER; BEGIN V = 1; N = :V; SUSPEND; END"
	f := newParameterFixture(t, body, nil)

	if _, err := f.execute(t, nil, nil); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	calls := f.backend.calls()
	if len(calls) == 0 {
		t.Fatal("the PSQL batch never reached the repository")
	}
	for i, call := range calls {
		if call.Args != nil {
			t.Errorf("statement %d took the bound path with %#v, want the legacy method", i+1, call.Args)
		}
	}
}

func TestParameterExplainTranslatesMarkersWithoutExecuting(t *testing.T) {
	f := newParameterFixture(t, "SELECT :ID FROM T", nil)

	var got string
	if err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExplainQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call explainQuery:", err)
	}

	explains := f.backend.explains()
	if len(explains) != 1 {
		t.Fatalf("ExplainPlan calls = %#v, want one prepared statement", explains)
	}
	if strings.Contains(explains[0], ":ID") || !strings.Contains(explains[0], "?") {
		t.Errorf("prepared SQL = %q, want the named marker rewritten to a positional one", explains[0])
	}
	// Explain is prepare-only: values are not needed to prepare a plan, so
	// nothing is prompted for and nothing is executed.
	requireNoCalls(t, f.backend)
	if !strings.Contains(got, "PLAN") {
		t.Errorf("result = %q, want the plan text", got)
	}
}

func TestParameterExplainKeepsPreparedOnlyRefusalsForMarkedStatements(t *testing.T) {
	f := newParameterFixture(t, "UPDATE T SET A = :A WHERE B = :B", nil)

	var got string
	if err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExplainQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call explainQuery:", err)
	}

	if !strings.Contains(got, explainPreparedOnly) {
		t.Errorf("result = %q, want the prepared-only banner", got)
	}
	requireNoCalls(t, f.backend)
}

func TestParameterExplainZeroMarkerStatementKeepsItsExistingPath(t *testing.T) {
	f := newParameterFixture(t, "SELECT 1 FROM T", nil)

	var got string
	if err := f.tx.conn.Call(f.tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExplainQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call explainQuery:", err)
	}

	if explains := f.backend.explains(); len(explains) != 1 || !strings.Contains(explains[0], "SELECT 1") {
		t.Fatalf("ExplainPlan calls = %#v, want the statement prepared verbatim", explains)
	}
	if !strings.Contains(got, "PLAN") {
		t.Errorf("result = %q, want the plan text", got)
	}
	requireNoCalls(t, f.backend)
}

// A procedure descriptor set is only meaningful if the catalog actually
// resolved it; otherwise every routing assertion above would pass for the
// wrong reason.
func TestParameterExecutionProcedureCatalogIsResolved(t *testing.T) {
	f := newParameterFixture(t, "EXECUTE PROCEDURE MYPROC(:CODE)", withProcedures)
	cache := f.tx.server.worker.Cache()
	if !cache.HasCatalog() {
		t.Fatal("the catalog never arrived; the routing assertions would be vacuous")
	}
	if desc, ok := cache.Procedure("MYPROC"); !ok || len(desc.OutputParameters) == 0 {
		t.Fatalf("MYPROC = (%+v, %v), want a cached descriptor with output", desc, ok)
	}
}
