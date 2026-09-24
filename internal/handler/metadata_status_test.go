package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func newMetadataStatusRPC(t *testing.T, advertise bool) (*Server, *jsonrpc2.Conn, <-chan string) {
	t.Helper()
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	clientMethods := make(chan string, 32)
	clientHandler := jsonrpc2.HandlerWithError(func(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
		select {
		case clientMethods <- req.Method:
		default:
		}
		return nil, nil
	})
	clientSide, serverSide := net.Pipe()
	ctx := context.Background()
	client := jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), clientHandler)
	server := jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	params := lsp.InitializeParams{}
	params.Capabilities.Window.WorkDoneProgress = advertise
	var result lsp.InitializeResult
	if err := client.Call(ctx, "initialize", params, &result); err != nil {
		t.Fatal(err)
	}
	return s, client, clientMethods
}

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

func TestMetadataStatusNeverPushesProgressForEitherCapability(t *testing.T) {
	for _, advertise := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-capability", true: "with-capability"}[advertise], func(t *testing.T) {
			s, client, clientMethods := newMetadataStatusRPC(t, advertise)
			ctx := context.Background()
			s.stateMu.Lock()
			s.initialized = true
			s.connectionState = connectionFailed
			s.connGeneration = 17
			s.stateMu.Unlock()

			var status MetadataStatusResult
			if err := client.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
				t.Fatal(err)
			}
			if !status.Settled || !status.Degraded || status.ConnectionErrorCode != "connection_failed" {
				t.Fatalf("status = %+v", status)
			}
			// A superseding idle generation and Stop remain pull-only and report
			// their own current state rather than the old failed attachment.
			s.stateMu.Lock()
			s.connectionState = connectionIdle
			s.connGeneration = 18
			s.stateMu.Unlock()
			if err := client.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
				t.Fatal(err)
			}
			if status.Generation != 18 || status.Settled || status.Degraded {
				t.Fatalf("idle status = %+v", status)
			}
			if err := s.Stop(); err != nil {
				t.Fatal(err)
			}
			if err := client.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
				t.Fatal(err)
			}
			if status.ConnectionState != string(connectionStopped) || status.Settled || status.Degraded {
				t.Fatalf("stopped status = %+v", status)
			}
			select {
			case method := <-clientMethods:
				if method == "window/workDoneProgress/create" || method == "$/progress" || method == "window/logMessage" {
					t.Fatalf("unexpected metadata push %q", method)
				}
			case <-time.After(100 * time.Millisecond):
			}
			for {
				select {
				case method := <-clientMethods:
					if method == "window/workDoneProgress/create" || method == "$/progress" || method == "window/logMessage" {
						t.Fatalf("unexpected metadata push %q", method)
					}
				default:
					return
				}
			}
		})
	}
}

func TestMetadataStatusStartFailureIsNotConnectionFailure(t *testing.T) {
	s, client, _ := newMetadataStatusRPC(t, true)
	s.stateMu.Lock()
	s.connectionState = connectionReady
	s.connGeneration = 9
	s.stateMu.Unlock()
	s.metadata.Reset(9)
	if err := s.metadata.MarkStartFailed(9); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.metadataStartErr = errors.New("fake password secret-from-driver")
	s.stateMu.Unlock()
	var status MetadataStatusResult
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	if status.ConnectionErrorCode != "" || status.MetadataErrorCode != "metadata_load_failed" || !status.Settled || !status.Degraded {
		t.Fatalf("start failure status = %+v", status)
	}
	if len(status.Categories) != 0 {
		t.Fatalf("start failure fabricated categories: %+v", status.Categories)
	}
	body, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("secret-from-driver")) {
		t.Fatalf("raw error leaked: %s", body)
	}
}

type metadataStatusFailureRepository struct{ *database.MockDBRepository }

func (metadataStatusFailureRepository) MetadataPlan() database.MetadataPlan {
	return database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{
		{Kind: database.MetadataSchemas, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, errors.New("secret-from-driver")
		}},
		{Kind: database.MetadataRelations, DependsOn: []database.MetadataKind{database.MetadataSchemas}, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, nil
		}},
	}}
}

func TestMetadataStatusFailedAndBlockedJobsOverJSONRPC(t *testing.T) {
	s, client, methods := newMetadataStatusRPC(t, true)
	s.stateMu.Lock()
	s.connectionState = connectionReady
	s.connGeneration = 22
	s.stateMu.Unlock()
	s.metadata.Reset(22)
	load, err := s.metadata.Start(context.Background(), 22, metadataStatusFailureRepository{&database.MockDBRepository{}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-load.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata jobs did not settle")
	}
	var got MetadataStatusResult
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Settled || !got.Degraded || len(got.Categories) != len(metadataKindOrder) {
		t.Fatalf("status = %+v", got)
	}
	if got.Categories[0].Kind != string(database.MetadataSchemas) || got.Categories[0].ErrorCode != "metadata_load_failed" || got.Categories[1].Kind != string(database.MetadataRelations) || got.Categories[1].ErrorCode != "dependency_failed" {
		t.Fatalf("category order/status = %+v", got.Categories)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("secret-from-driver")) {
		t.Fatalf("raw error leaked: %s", body)
	}
	select {
	case method := <-methods:
		if method == "window/workDoneProgress/create" || method == "$/progress" || method == "window/logMessage" {
			t.Fatalf("unexpected metadata push %q", method)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMetadataStatusQueuedCancellationHasZeroRunDuration(t *testing.T) {
	s, _, _ := newMetadataStatusRPC(t, false)
	s.stateMu.Lock()
	s.connectionState = connectionReady
	s.connGeneration = 23
	s.stateMu.Unlock()
	s.metadata.Reset(23)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	load, err := s.metadata.Start(ctx, 23, metadataStatusPlanRepository{MockDBRepository: &database.MockDBRepository{}, plan: metadataStatusPlanWithQueuedJob(started)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("first metadata job did not start")
	}
	cancel()
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled metadata generation did not settle")
	}
	result := s.metadataStatus()
	for _, category := range result.Categories {
		if category.Kind == string(database.MetadataRelations) && category.DurationMS != 0 {
			t.Fatalf("queued cancellation duration = %dms, want zero", category.DurationMS)
		}
	}
}

type metadataStatusPlanRepository struct {
	*database.MockDBRepository
	plan database.MetadataPlan
}

func (r metadataStatusPlanRepository) MetadataPlan() database.MetadataPlan { return r.plan }

func metadataStatusPlanWithQueuedJob(started chan<- struct{}) database.MetadataPlan {
	return database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{
		{Kind: database.MetadataSchemas, Run: func(ctx context.Context, _ *database.DBCache) (database.MetadataPatch, error) {
			close(started)
			<-ctx.Done()
			return database.MetadataPatch{}, ctx.Err()
		}},
		{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, nil
		}},
	}}
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
