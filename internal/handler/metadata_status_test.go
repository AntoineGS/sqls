package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/internal/lsp"
)

func TestMetadataStatusCommandIsAdvertised(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	var got lsp.InitializeResult
	if err := tx.conn.Call(tx.ctx, "initialize", lsp.InitializeParams{Capabilities: lsp.ClientCapabilities{Window: lsp.WindowClientCapabilities{WorkDoneProgress: true}}}, &got); err != nil {
		t.Fatal(err)
	}
	commands := got.Capabilities.ExecuteCommandProvider.Commands
	found := false
	for _, command := range commands {
		if command == CommandShowMetadataStatus {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q not advertised: %v", CommandShowMetadataStatus, commands)
	}
}

func TestMetadataStatusIsNonblockingAndDoesNotLeakErrors(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.coordinator.Stop()
	<-tx.server.coordinator.done
	tx.server.stateMu.Lock()
	tx.server.connectionState = connectionFailed
	tx.server.connGeneration = 8
	tx.server.stateMu.Unlock()
	tx.server.metadataStartErr = errors.New("driver rejected secret-from-driver")

	var got MetadataStatusResult
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &got); err != nil {
		t.Fatal(err)
	}
	if got.ConnectionErrorCode != "connection_failed" || !got.Settled || !got.Degraded {
		t.Fatalf("unexpected failed status: %+v", got)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("secret-from-driver")) {
		t.Fatalf("raw error leaked: %s", body)
	}
}
