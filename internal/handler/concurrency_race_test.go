package handler

import (
	"fmt"
	"testing"
	"time"

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
