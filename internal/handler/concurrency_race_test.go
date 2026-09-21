package handler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/internal/lsp"
)

// This test is meaningful under -race. It parks executeQuery after it has read
// the document text and then rewrites that text from the inline read loop,
// which is the unsynchronised write/read pair the copy rule closes.
//
// The wait below is a sleep, not gate.waitEntered. executeQuery reads f.Text at
// execute_command.go:141 *before* it reaches repo.Query, so receiving the gate's
// signal would order that read ahead of every didChange this test then sends and
// the detector would see no race at all.
func TestDidChangeDuringAsyncQueryDoesNotRaceOnFileText(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	commandDone := make(chan error, 1)
	go func() {
		var got string
		commandDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got)
	}()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 50; i++ {
		params := lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: i + 1},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{
				{Text: fmt.Sprintf("SELECT %d;", i)},
			},
		}
		if err := tx.conn.Call(tx.ctx, "textDocument/didChange", params, nil); err != nil {
			t.Fatal("conn.Call textDocument/didChange:", err)
		}
	}

	gate.release()
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight command never completed")
	}

	if got, ok := tx.server.fileText(testFileURI); !ok || got != "SELECT 49;" {
		t.Fatalf("fileText = (%q, %v), want (\"SELECT 49;\", true)", got, ok)
	}
}

func TestSwitchConnectionWaitsForInFlightQuery(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")

	queryDone := make(chan string, 1)
	queryErr := make(chan error, 1)
	go func() {
		var got string
		if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got); err != nil {
			queryErr <- err
			return
		}
		queryDone <- got
	}()
	gate.waitEntered(t)

	switchDone := make(chan error, 1)
	go func() {
		switchDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandSwitchConnection,
			Arguments: []interface{}{"2"},
		}, nil)
	}()

	select {
	case err := <-switchDone:
		t.Fatalf("switchConnections completed while a query was in flight (err=%v)", err)
	case err := <-queryErr:
		t.Fatal("conn.Call workspace/executeCommand:", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: the switch is blocked behind the in-flight query.
	}

	gate.release()

	select {
	case got := <-queryDone:
		if !strings.Contains(got, "42") {
			t.Errorf("query result = %q, want the intact row value 42", got)
		}
	case err := <-queryErr:
		t.Fatal("in-flight query failed:", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight query never completed")
	}

	select {
	case err := <-switchDone:
		if err != nil {
			t.Fatal("conn.Call switchConnections:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("switchConnections never completed")
	}
}

func TestQueryAfterSwitchUsesNewConnection(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandSwitchConnection,
		Arguments: []interface{}{"2"},
	}, nil); err != nil {
		t.Fatal("conn.Call switchConnections:", err)
	}

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	opened := backend.opened()
	if len(opened) < 2 {
		t.Fatalf("opened %d connections, want at least 2", len(opened))
	}
	newest := opened[len(opened)-1]

	queries := backend.queries()
	if len(queries) != 1 {
		t.Fatalf("repository served %d queries, want 1", len(queries))
	}
	if queries[0].db != newest {
		t.Error("the query ran against a stale connection, want the newest one")
	}
}

// switchDatabase reads WSCfg through getConfig and then parks inside the cache
// rebuild, so an inline didChangeConfiguration writes WSCfg while the async
// command's read is still unordered against it. Meaningful under -race.
//
// As in the file-text test, the wait is a sleep rather than gate.waitEntered:
// getConfig reads WSCfg at handler.go:423, well before the gated CurrentSchema
// call, so receiving the gate's signal would order the read ahead of the write
// below and hide the race.
func TestWorkspaceConfigurationChangeDuringAsyncCommandDoesNotRace(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))

	gate := backend.gate("CurrentSchema")
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandSwitchDatabase,
			Arguments: []interface{}{"other"},
		}, nil)
	}()
	time.Sleep(200 * time.Millisecond)

	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))

	gate.release()
	select {
	case <-commandDone:
	case <-time.After(10 * time.Second):
		t.Fatal("switchDatabase never completed")
	}
}

func TestCancelledQueryRendersTheCancelledMessage(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	requestID := jsonrpc2.ID{Str: "render-cancel", IsString: true}

	type callResult struct {
		out string
		err error
	}
	done := make(chan callResult, 1)
	go func() {
		var got string
		err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got, jsonrpc2.PickID(requestID))
		done <- callResult{out: got, err: err}
	}()
	gate.waitEntered(t)

	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", res.err)
		}
		// This is an untagged build, so ClassifyFailure reports FailureNone and
		// the notice degrades to the stopped-waiting wording.
		if !strings.Contains(res.out, "sqls stopped waiting for this statement") {
			t.Errorf("result = %q, want the stopped-waiting message", res.out)
		}
		if strings.Contains(res.out, "42") {
			t.Errorf("result = %q, want no rows for a cancelled statement", res.out)
		}
		// The regression assertion: a cancelled statement must never also be
		// told that it completed and the cancellation arrived too late.
		if strings.Contains(res.out, "arrived after the statement completed") {
			t.Errorf("result = %q, want no late-arrival note on a cancelled statement", res.out)
		}
		if strings.Contains(res.out, lateCancellationNote) {
			t.Errorf("result = %q, want the late-cancellation note absent", res.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled command never completed")
	}
}
