package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestStopSchedulesCleanupWhenConnectionCloseFails(t *testing.T) {
	closeErr := errors.New("attachment is half dead")
	server := NewServer()
	server.dbConn = &database.DBConnection{
		Driver: "stub",
		Tunnel: failingCloser{err: closeErr},
	}

	if err := server.Stop(); err != nil {
		t.Fatalf("Stop() scheduling error = %v", err)
	}
	select {
	case <-server.cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup worker did not finish")
	}
	if server.connectionState != connectionStopped {
		t.Fatal("server did not enter terminal stopped state")
	}
}

func TestExecuteCommandDispatchesAsynchronously(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.Stop()

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

func TestExecuteQueryHonoursCancelRequest(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	requestID := jsonrpc2.ID{Str: "cancel-me", IsString: true}

	done := make(chan error, 1)
	go func() {
		var got string
		done <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got, jsonrpc2.PickID(requestID))
	}()
	gate.waitEntered(t)

	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case <-done:
		// Any outcome is acceptable here; the assertion is that the call
		// returned promptly and the repository saw a cancelled context.
	case <-time.After(5 * time.Second):
		t.Fatal("$/cancelRequest did not unblock the in-flight query")
	}

	if !gate.contextWasCancelled() {
		t.Error("the repository never observed a cancelled context")
	}
}

func TestCancelRequestForUnknownIDIsIgnored(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.Stop()

	unknown := jsonrpc2.ID{Str: "no-such-request", IsString: true}
	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: unknown}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	// The server must still be serving.
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")
}

func TestLateCancellationRendersTheRealResultWithANote(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	// This gate lets the statement succeed once the context is cancelled, which
	// is the driver's "the cancellation arrived too late" case.
	gate := backend.gateIgnoringCancel("Query")
	requestID := jsonrpc2.ID{Str: "late-cancel", IsString: true}

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
		if !strings.Contains(res.out, "42") {
			t.Errorf("result = %q, want the real row value 42", res.out)
		}
		if !strings.Contains(res.out, "the cancellation request arrived after the statement completed") {
			t.Errorf("result = %q, want the late-cancellation note", res.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command never completed")
	}
}
