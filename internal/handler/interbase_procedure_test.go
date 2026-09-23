package handler

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestInterBaseProcedureName(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "call with parenthesised arguments", query: "EXECUTE PROCEDURE MYPROC(1, 2)", want: "MYPROC"},
		{name: "call with no parenthesis", query: "EXECUTE PROCEDURE MYPROC 1 2", want: "MYPROC"},
		{name: "no arguments", query: "EXECUTE PROCEDURE MYPROC", want: "MYPROC"},
		{name: "trailing semicolon", query: "EXECUTE PROCEDURE MYPROC;", want: "MYPROC"},
		{name: "lower case keywords and name", query: "execute procedure myproc(1)", want: "myproc"},
		{name: "quoted name", query: `EXECUTE PROCEDURE "MyProc"(1)`, want: "MyProc"},
		{name: "dollar in name", query: "EXECUTE PROCEDURE MY$PROC", want: "MY$PROC"},
		{name: "extra whitespace", query: "EXECUTE\tPROCEDURE\n  MYPROC", want: "MYPROC"},
		{name: "not a procedure call", query: "SELECT * FROM MYPROC", want: ""},
		{name: "execute without procedure", query: "EXECUTE STMT", want: ""},
		{name: "execute procedure with no name", query: "EXECUTE PROCEDURE", want: ""},
		{name: "empty", query: "", want: ""},
		// Known limitation, recorded rather than worked around: a Dialect-3
		// quoted name containing whitespace does not survive field splitting.
		// It degrades to "", which routes to Exec — today's behaviour — rather
		// than to a wrong decision.
		{name: "quoted name with a space", query: `EXECUTE PROCEDURE "My Proc"(1)`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := interBaseProcedureName(tt.query); got != tt.want {
				t.Errorf("interBaseProcedureName(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func testProcedures() []*database.ProcedureDesc {
	return []*database.ProcedureDesc{
		{
			Name: "MYPROC",
			InputParameters: []*database.ProcedureParameterDesc{
				{Name: "IN_CODE", Position: 0, Direction: database.ParameterInput, Type: "VARCHAR(3)"},
			},
			OutputParameters: []*database.ProcedureParameterDesc{
				{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
			},
		},
		{
			Name: "DOWORK",
			InputParameters: []*database.ProcedureParameterDesc{
				{Name: "IN_ID", Position: 0, Direction: database.ParameterInput, Type: "INTEGER"},
			},
		},
	}
}

func runProcedureCommand(t *testing.T, text string, procs []*database.ProcedureDesc) (string, *stubBackend) {
	return runProcedureCommandWithProcedureState(t, text, procs, nil)
}

func runProcedureCommandWithProcedureState(t *testing.T, text string, procs []*database.ProcedureDesc, procedureState *database.MetadataState) (string, *stubBackend) {
	t.Helper()
	tx := newTestContext()
	tx.setup(t)
	t.Cleanup(tx.tearDown)
	t.Cleanup(tx.server.worker.Stop)

	backend := installStubBackend(t)
	if procs != nil {
		backend.setProcedures(procs)
	}
	tx.addWorkspaceConfig(t, stubInterBaseConnections("interbase"))

	// The catalog lands on the worker's SECONDARY, asynchronous pass:
	// addWorkspaceConfig reaches ReCache, which only signals the worker
	// goroutine (worker.go:95-97). Issuing the command straight afterwards
	// races that goroutine, HasCatalog() is still false, and routing falls to
	// the unknown-procedure branch — so the two tests that assert the Query
	// path would fail or, worse, flake. Wait for the catalog first.
	//
	// waitForCatalog is the polling helper the catalog-migration plan adds
	// alongside GenerateCatalogCache. If that plan has not landed, add it
	// there rather than duplicating it here.
	if procs != nil {
		waitForCatalog(t, tx.server.worker)
		cache := tx.server.worker.Cache()
		if !cache.HasCatalog() {
			t.Fatal("the catalog never arrived; every routing assertion below would be vacuous")
		}
		if procedureState != nil {
			cache.Metadata[database.MetadataProcedures] = *procedureState
		}
	}

	tx.textDocumentDidOpen(t, testFileURI, text)

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}
	return got, backend
}

func TestExecuteProcedureWithFailedProcedureMetadataRemainsUnknown(t *testing.T) {
	failed := database.MetadataFailed
	got, backend := runProcedureCommandWithProcedureState(t, "EXECUTE PROCEDURE MYPROC(1);", []*database.ProcedureDesc{{Name: "OTHER"}}, &failed)
	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements without a known procedure signature", len(queries))
	}
	if !strings.Contains(got, "MYPROC is not in the catalog cache") || !strings.Contains(got, "switch to this connection again") {
		t.Errorf("result = %q, want existing unknown-metadata explanation", got)
	}
	if strings.Contains(got, "no output") || strings.Contains(got, "has no output") {
		t.Errorf("result = %q, failed metadata must not be described as a procedure without outputs", got)
	}
}

func TestExecuteProcedureUsesKnownDescriptorWhileCategoryFailed(t *testing.T) {
	failed := database.MetadataFailed
	got, backend := runProcedureCommandWithProcedureState(t, "EXECUTE PROCEDURE MYPROC(1);", testProcedures(), &failed)
	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements for a present descriptor, want 1", len(queries))
	}
	if strings.Contains(got, "not in the catalog cache") {
		t.Errorf("known descriptor was discarded because category state is failed: %q", got)
	}
}

func TestExecuteProcedureWithOutputUsesQueryPath(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE MYPROC(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements, want 1 — a procedure with output must take the Query path", len(queries))
	}
	if served := backend.readOnlyQueries(); len(served) != 0 {
		t.Errorf("QueryReadOnly served %d statements, want 0 — EXECUTE PROCEDURE is a write even when it returns a row", len(served))
	}
	if !strings.Contains(got, "EXECUTE PROCEDURE returns at most one row.") {
		t.Errorf("result = %q, want the one-row note", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rendered output row", got)
	}
}

func TestExecuteProcedureWithoutOutputUsesExecPath(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE DOWORK(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 — a procedure with no output must take the Exec path", len(queries))
	}
	if !strings.Contains(got, "Query OK") {
		t.Errorf("result = %q, want the Exec result line", got)
	}
	if strings.Contains(got, "EXECUTE PROCEDURE returns at most one row.") {
		t.Errorf("result = %q, want no one-row note on the Exec path", got)
	}
}

func TestExecuteProcedureRoutingIsCaseInsensitive(t *testing.T) {
	// InterBase stores catalog names upper-cased; users type them lower-cased.
	_, backend := runProcedureCommand(t, "execute procedure myproc(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements, want 1 — a lower-case name must still resolve to MYPROC", len(queries))
	}
}

func TestExecuteProcedureUnknownProcedureUsesExecAndExplainsCacheRefresh(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE NOSUCHPROC(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 — an unknown procedure must never be tried on both paths", len(queries))
	}
	if !strings.Contains(got, "NOSUCHPROC is not in the catalog cache") {
		t.Errorf("result = %q, want the cache-refresh hint naming the procedure", got)
	}
	if !strings.Contains(got, "switch to this connection again to refresh the cache") {
		t.Errorf("result = %q, want the refresh instruction", got)
	}
}

func TestExecuteProcedureWithoutCatalogUsesExecPath(t *testing.T) {
	// The window before the worker's catalog pass lands: HasCatalog() is false
	// and routing falls back to today's unconditional Exec, which is not a
	// regression.
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE MYPROC(1);", nil)

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 without a catalog", len(queries))
	}
	if !strings.Contains(got, "Query OK") && !strings.Contains(got, "is not in the catalog cache") {
		t.Errorf("result = %q, want either the Exec result or the refresh hint", got)
	}
}

func TestExecuteProcedureRoutingIgnoresNonInterBaseDrivers(t *testing.T) {
	// This test is only meaningful if the non-InterBase connection HAS a
	// catalog. If it does not, routing returns unknown for want of a cache,
	// takes the Exec path, and the assertion below passes for the wrong
	// reason — deleting the parserDriver() check from
	// interBaseProcedureRouting would leave it green. Step 2 therefore
	// teaches the plain stubDriverName factory the same catalogStubRepository
	// wrap the InterBase one gets, so the ONLY difference between this test
	// and TestExecuteProcedureWithOutputUsesQueryPath is the driver.
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	backend.setProcedures(testProcedures())
	tx.addWorkspaceConfig(t, stubConnections("primary"))

	waitForCatalog(t, tx.server.worker)
	if !tx.server.worker.Cache().HasCatalog() {
		t.Fatal("the non-InterBase connection has no catalog; this test would pass for the wrong reason")
	}
	if _, ok := tx.server.worker.Cache().Procedure("MYPROC"); !ok {
		t.Fatal("MYPROC is not in the cache; routing would return unknown regardless of the driver")
	}

	tx.textDocumentDidOpen(t, testFileURI, "EXECUTE PROCEDURE MYPROC(1);")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements on a non-InterBase driver, want 0 — routing must be InterBase-only", len(queries))
	}
	// And no cache-refresh hint either: an unknown procedure is an InterBase
	// concept, so a non-InterBase driver must produce today's plain output.
	if strings.Contains(got, "is not in the catalog cache") {
		t.Errorf("result = %q, want no InterBase routing hint on a non-InterBase driver", got)
	}
}
