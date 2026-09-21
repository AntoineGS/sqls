package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestStopStopsWorkerEvenWhenConnectionCloseFails(t *testing.T) {
	closeErr := errors.New("attachment is half dead")
	server := NewServer()
	server.dbConn = &database.DBConnection{
		Driver: "stub",
		Tunnel: failingCloser{err: closeErr},
	}

	if err := server.Stop(); !errors.Is(err, closeErr) {
		t.Fatalf("Stop() = %v, want %v", err, closeErr)
	}

	// A stopped worker must not accept another update. Stop is idempotent, so
	// calling it again is the cheap way to assert it already ran: a worker that
	// was never stopped would still be running here and the select below would
	// not see a closed channel.
	select {
	case <-server.worker.Done():
	default:
		t.Fatal("Stop returned without stopping the worker")
	}
}

func TestExecuteCommandDispatchesAsynchronously(t *testing.T) {
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
	gate.waitEntered(t)

	// The command is parked inside the repository. A read-only request must
	// still be served; with inline dispatch this call times out.
	formatCtx, cancel := context.WithTimeout(tx.ctx, 5*time.Second)
	defer cancel()
	var edits []lsp.TextEdit
	if err := tx.conn.Call(formatCtx, "textDocument/formatting", lsp.DocumentFormattingParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
	}, &edits); err != nil {
		t.Fatal("second request was not served while a command was in flight:", err)
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
}
