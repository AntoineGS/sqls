package handler

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestNeovimFixtureSnapshotIsDeterministic(t *testing.T) {
	registry := metadata.NewRegistry(context.Background(), metadata.Options{})
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	provider := &neovimFixtureProvider{variant: "old"}
	lease, err := registry.Acquire(context.Background(), metadata.Key{Engine: "fixture", URL: "fixture://old"}, metadata.Scope{Principal: "fixture"}, provider.factory)
	if err != nil {
		t.Fatal("acquire fixture metadata lease:", err)
	}
	t.Cleanup(lease.Release)
	if err := lease.Refresh(context.Background()); err != nil {
		t.Fatal("start fixture metadata lease:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("fixture metadata readiness: %v; snapshot=%+v", err, lease.Snapshot())
	}
	cache := lease.Snapshot().Cache
	if cache == nil || !cache.HasCatalog() {
		t.Fatal("fixture cache must include a catalog")
	}
	columns, ok := cache.ColumnDescs("CUSTOMERS")
	if !ok || len(columns) != 3 || columns[0].Name != "CUSTOMER_ID" || columns[0].Key != "YES" {
		t.Fatalf("CUSTOMERS columns = %#v, want ordered fixture columns and primary key", columns)
	}
	procedure, ok := cache.Procedure("FIND_CUSTOMER")
	if !ok || len(procedure.InputParameters) != 1 || procedure.InputParameters[0].Name != "P_CUSTOMER_ID" {
		t.Fatalf("FIND_CUSTOMER signature = %#v, want one P_CUSTOMER_ID input", procedure)
	}
	if index, ok := cache.Catalog.Indexes["PK_CUSTOMERS"]; !ok || index.RelationName != "CUSTOMERS" || len(index.Columns) != 1 || index.Columns[0] != "CUSTOMER_ID" {
		t.Fatalf("PK_CUSTOMERS = %#v, want one CUSTOMER_ID segment on CUSTOMERS", index)
	}
	if !strings.Contains(neovimFixtureTableDDL, "CREATE TABLE CUSTOMERS") || !strings.Contains(neovimFixtureProcedureSource, "SUSPEND") {
		t.Fatal("fixture DDL/source contract is incomplete")
	}
	for _, kind := range []catalog.MetadataKind{database.MetadataRelations, database.MetadataColumnsCurrent, database.MetadataPrimaryKeys, database.MetadataProcedures, database.MetadataViews} {
		if !cache.MetadataReady(kind) {
			t.Errorf("fixture category %q is not ready", kind)
		}
	}
	newProvider := &neovimFixtureProvider{variant: "new"}
	newLease, err := registry.Acquire(context.Background(), metadata.Key{Engine: "fixture", URL: "fixture://new"}, metadata.Scope{Principal: "fixture"}, newProvider.factory)
	if err != nil {
		t.Fatal("acquire new-target fixture lease:", err)
	}
	t.Cleanup(newLease.Release)
	if err := newLease.Refresh(context.Background()); err != nil {
		t.Fatal("start new-target fixture metadata lease:", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := metadatatest.WaitReady(ctx, newLease); err != nil {
		t.Fatal("new-target metadata readiness:", err)
	}
	newName, _ := newLease.Snapshot().Cache.ColumnDescs("CUSTOMERS")
	if len(newName) != 3 || newName[1].Type != "VARCHAR(100)" || columns[1].Type != "VARCHAR(80)" {
		t.Fatalf("old/new fixture target values = %v / %v, want VARCHAR(80) / VARCHAR(100)", columns, newName)
	}
}

func TestNeovimFixtureProcedureLocalNavigationAndSymbols(t *testing.T) {
	request := language.Request{Document: language.Document{URI: "file:///fixture-local.sql", Text: neovimFixtureProcedureBuffer, Version: 1}, Context: language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}}}
	service := language.NewService()
	refs, err := service.References(context.Background(), request, language.Position{Line: 4, UTF16Column: 29}, true)
	if err != nil {
		t.Fatal("procedure-local references:", err)
	}
	wantRanges := []language.Range{
		{Start: language.Position{Line: 1, UTF16Column: 17}, End: language.Position{Line: 1, UTF16Column: 18}},
		{Start: language.Position{Line: 3, UTF16Column: 0}, End: language.Position{Line: 3, UTF16Column: 1}},
		{Start: language.Position{Line: 4, UTF16Column: 0}, End: language.Position{Line: 4, UTF16Column: 1}},
		{Start: language.Position{Line: 4, UTF16Column: 5}, End: language.Position{Line: 4, UTF16Column: 6}},
		{Start: language.Position{Line: 4, UTF16Column: 29}, End: language.Position{Line: 4, UTF16Column: 30}},
	}
	if len(refs.Value) != len(wantRanges) {
		t.Fatalf("procedure-local references = %#v, want %d declaration/use spans", refs.Value, len(wantRanges))
	}
	for i, want := range wantRanges {
		if refs.Value[i].Range != want {
			t.Errorf("reference %d range = %+v, want %+v", i, refs.Value[i].Range, want)
		}
	}
	rename, err := service.Rename(context.Background(), request, language.Position{Line: 4, UTF16Column: 29}, "V_AMOUNT")
	if err != nil {
		t.Fatal("procedure-local rename:", err)
	}
	if len(rename.Value) != 1 || len(rename.Value[0].Edits) != len(wantRanges) {
		t.Fatalf("procedure-local rename = %#v, want one workspace edit with %d spans", rename.Value, len(wantRanges))
	}
	for i, want := range wantRanges {
		edit := rename.Value[0].Edits[i]
		if edit.Range != want || edit.Text != "V_AMOUNT" {
			t.Errorf("rename edit %d = %+v, want span %+v replacing with V_AMOUNT", i, edit, want)
		}
	}

	symbolRequest := request
	symbolRequest.Document = language.Document{URI: "file:///fixture-symbols.sql", Text: neovimFixtureProcedureSymbols, Version: 1}
	symbols, err := service.Symbols(context.Background(), symbolRequest)
	if err != nil {
		t.Fatal("procedure document symbols:", err)
	}
	found := false
	for _, symbol := range symbols.Value {
		if symbol.Name == "X" && symbol.Kind == language.SymbolKindVariable {
			found = true
		}
	}
	if !found {
		t.Fatalf("procedure fixture symbols = %#v, want local variable X", symbols.Value)
	}
	for _, symbol := range symbols.Value {
		if symbol.Name == "X" && (symbol.Range != (language.Range{Start: language.Position{Line: 0, UTF16Column: 0}, End: language.Position{Line: 0, UTF16Column: 66}}) || symbol.SelectionRange != (language.Range{Start: language.Position{Line: 0, UTF16Column: 39}, End: language.Position{Line: 0, UTF16Column: 40}})) {
			t.Errorf("variable symbol = %+v, want scope [0:0,0:66] and selection [0:39,0:40]", symbol)
		}
	}
}

func TestNeovimFixtureDocumentSymbolOverRawStdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client := startFixtureRawClient(t, ctx)
	defer client.stop()

	initParams := map[string]any{"initializationOptions": map[string]any{"connectionConfig": map[string]any{
		"alias": "old", "driver": string(neovimFixtureDriver), "dataSourceName": "fixture://old",
	}}}
	if err := client.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": initParams}); err != nil {
		t.Fatal("write initialize:", err)
	}
	initialized, err := client.waitResponse(ctx, 1)
	if err != nil {
		t.Fatal("initialize response:", err)
	}
	var init struct {
		Capabilities struct {
			DocumentSymbolProvider  bool `json:"documentSymbolProvider"`
			WorkspaceSymbolProvider bool `json:"workspaceSymbolProvider"`
			FoldingRangeProvider    bool `json:"foldingRangeProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(initialized.Result, &init); err != nil {
		t.Fatal("decode initialize result:", err)
	}
	if !init.Capabilities.DocumentSymbolProvider || init.Capabilities.WorkspaceSymbolProvider || init.Capabilities.FoldingRangeProvider {
		t.Fatalf("outline capabilities = %+v, want document symbols only", init.Capabilities)
	}
	if err := client.write(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		t.Fatal("write initialized:", err)
	}
	if err := client.waitReady(ctx); err != nil {
		t.Fatal("wait for fixture readiness:", err)
	}
	uri := "file:///fixture-symbols.sql"
	if err := client.write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": neovimFixtureProcedureSymbols},
	}}); err != nil {
		t.Fatal("write didOpen:", err)
	}
	if err := client.write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "textDocument/documentSymbol", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}}); err != nil {
		t.Fatal("write documentSymbol:", err)
	}
	response, err := client.waitResponse(ctx, 2)
	if err != nil {
		t.Fatal("documentSymbol response:", err)
	}
	if len(response.Error) != 0 {
		t.Fatalf("documentSymbol JSON-RPC error: %s", response.Error)
	}
	var symbols []documentSymbolResult
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal("decode document symbols:", err)
	}
	if len(symbols) != 1 || symbols[0].Name != "X" || symbols[0].Kind != 13 || symbols[0].SelectionRange.Start != (lsp.Position{Line: 0, Character: 39}) {
		t.Fatalf("fixture document symbols = %#v, want local X kind 13 at UTF-16 0:39", symbols)
	}
	if err := client.shutdown(ctx); err != nil {
		t.Fatal("shutdown fixture:", err)
	}
}

func TestNeovimFixtureCancellationWaitsForQueryEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client := startFixtureRawClient(t, ctx)
	defer client.stop()
	if err := client.initialize(ctx); err != nil {
		t.Fatal("initialize fixture child:", err)
	}
	if err := client.waitReady(ctx); err != nil {
		t.Fatal("wait for fixture metadata readiness:", err)
	}
	const (
		uri   = "file:///fixture-cancel.sql"
		query = "SELECT SQLS_FIXTURE_WAIT FROM CUSTOMERS"
	)
	client.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "sql", "version": 1, "text": query}})
	client.requestAsync(100, "workspace/executeCommand", map[string]any{"command": CommandExecuteQuery, "arguments": []string{uri}})
	entryCtx, entryCancel := context.WithTimeout(ctx, 2*time.Second)
	defer entryCancel()
	entered, err := client.waitNotification(entryCtx, "sqls/fixtureQueryEntered")
	if err != nil {
		t.Fatal("wait for gated query-entry notification:", err)
	}
	var gate struct {
		Marker string `json:"marker"`
		SQL    string `json:"sql"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(entered.Params, &gate); err != nil {
		t.Fatal("decode fixture query-entry notification:", err)
	}
	if gate.Marker != "SQLS_FIXTURE_WAIT" || gate.SQL != query || gate.Target != "fixture://old" {
		t.Fatalf("query-entry gate = %+v, want exact SQL marker and active fixture target", gate)
	}
	client.notify("$/cancelRequest", map[string]any{"id": 100})
	response, err := client.waitResponse(ctx, 100)
	if err != nil {
		t.Fatal("wait for cancelled execute response:", err)
	}
	if len(response.Error) != 0 {
		t.Fatalf("cancelled execute response has JSON-RPC error: %s", response.Error)
	}
	var result string
	if err := json.Unmarshal(response.Result, &result); err != nil || !strings.Contains(strings.ToLower(result), "cancel") {
		t.Fatalf("cancelled execute result = %q, decode err=%v; want cancellation notice", result, err)
	}
	if err := client.shutdown(ctx); err != nil {
		t.Fatal("shutdown fixture child:", err)
	}
}

func TestFixtureRawClientStopUnblocksFullMessageQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := startFixtureRawClient(t, ctx)
	defer client.stop()
	for range cap(client.messages) {
		client.messages <- fixtureRPCMessage{}
	}
	params := map[string]any{"initializationOptions": map[string]any{"connectionConfig": map[string]any{
		"alias": "old", "driver": string(neovimFixtureDriver), "dataSourceName": "fixture://old",
	}}}
	if err := client.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params}); err != nil {
		t.Fatal("write initialize to fixture child:", err)
	}
	select {
	case <-client.readPending:
	case <-ctx.Done():
		t.Fatal("fixture reader did not reach full message queue:", ctx.Err())
	}

	client.stop()
	if !isClosed(client.readDone) {
		// Leave the regression test's child and reader clean on the unfixed
		// implementation while preserving the failed assertion below.
		<-client.messages
		select {
		case <-client.readDone:
		case <-time.After(2 * time.Second):
			t.Fatal("fixture stdout reader remained blocked after draining queue")
		}
		client.stop()
		t.Fatal("stop did not unblock stdout reader blocked on a full message queue")
	}
	if !client.waited {
		t.Fatal("stop returned before reaping fixture child")
	}
}

type fixtureRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type fixtureRawClient struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	messages    chan fixtureRPCMessage
	readErr     chan error
	readDone    chan struct{}
	readPending chan struct{}
	readCancel  chan struct{}
	waitDone    chan error
	stderr      bytes.Buffer
	stderrEnd   chan struct{}
	writeMu     sync.Mutex
	waitOnce    sync.Once
	readOnce    sync.Once
	closed      bool
	waited      bool
}

func startFixtureRawClient(t *testing.T, ctx context.Context) *fixtureRawClient {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate current test binary:", err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestNeovimFixtureServer$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SQLS_NEOVIM_FIXTURE=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "SQLS_NEOVIM_FIXTURE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("create fixture child stdin:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("create fixture child stdout:", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal("create fixture child stderr:", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("start fixture test-binary child:", err)
	}
	c := &fixtureRawClient{cmd: cmd, stdin: stdin, messages: make(chan fixtureRPCMessage, 64), readErr: make(chan error, 1), readDone: make(chan struct{}), readPending: make(chan struct{}, 1), readCancel: make(chan struct{}), waitDone: make(chan error, 1), stderrEnd: make(chan struct{})}
	go func() {
		_, _ = io.Copy(&c.stderr, stderr)
		close(c.stderrEnd)
	}()
	go func() {
		c.readFrames(stdout)
		close(c.readDone)
	}()
	return c
}

func (c *fixtureRawClient) initialize(ctx context.Context) error {
	params := map[string]any{"initializationOptions": map[string]any{"connectionConfig": map[string]any{
		"alias": "old", "driver": string(neovimFixtureDriver), "dataSourceName": "fixture://old",
	}}}
	if err := c.request(1, "initialize", params); err != nil {
		return err
	}
	c.notify("initialized", map[string]any{})
	return nil
}

func (c *fixtureRawClient) waitReady(ctx context.Context) error {
	deadline := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	for id := int64(10); ; id++ {
		if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "workspace/executeCommand", "params": map[string]any{"command": CommandShowMetadataStatus}}); err != nil {
			return err
		}
		response, err := c.waitResponse(ctx, id)
		if err != nil {
			return err
		}
		var status struct {
			ConnectionState string `json:"connectionState"`
			Settled         bool   `json:"settled"`
			Degraded        bool   `json:"degraded"`
		}
		if len(response.Error) == 0 && json.Unmarshal(response.Result, &status) == nil && status.ConnectionState == string(connectionReady) && status.Settled && !status.Degraded {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixture never became ready (last status: %s): %w", response.Result, ctx.Err())
		case <-deadline.C:
		}
	}
}

func (c *fixtureRawClient) request(id int64, method string, params any) error {
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := c.waitResponse(ctx, id)
	if err != nil {
		return err
	}
	if len(response.Error) != 0 {
		return fmt.Errorf("%s returned JSON-RPC error: %s", method, response.Error)
	}
	return nil
}

func (c *fixtureRawClient) requestAsync(id int64, method string, params any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (c *fixtureRawClient) notify(method string, params any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *fixtureRawClient) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err = c.stdin.Write(payload)
	return err
}

func (c *fixtureRawClient) waitResponse(ctx context.Context, id int64) (fixtureRPCMessage, error) {
	want := strconv.FormatInt(id, 10)
	for {
		message, err := c.next(ctx)
		if err != nil {
			return fixtureRPCMessage{}, err
		}
		if string(message.ID) == want {
			return message, nil
		}
	}
}

func (c *fixtureRawClient) waitNotification(ctx context.Context, method string) (fixtureRPCMessage, error) {
	for {
		message, err := c.next(ctx)
		if err != nil {
			return fixtureRPCMessage{}, err
		}
		if message.Method == method {
			return message, nil
		}
	}
}

func (c *fixtureRawClient) next(ctx context.Context) (fixtureRPCMessage, error) {
	select {
	case message := <-c.messages:
		return message, nil
	case err := <-c.readErr:
		if err == nil {
			err = io.EOF
		}
		return fixtureRPCMessage{}, fmt.Errorf("fixture stdio stream ended: %w", err)
	case <-ctx.Done():
		return fixtureRPCMessage{}, ctx.Err()
	}
}

func (c *fixtureRawClient) readFrames(r io.Reader) {
	reader := bufio.NewReader(r)
	for {
		length := -1
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				c.readErr <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			name, value, ok := strings.Cut(line, ":")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				c.readErr <- fmt.Errorf("unexpected stdout outside JSON-RPC framing: %q", line)
				return
			}
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 || length > 8<<20 {
				c.readErr <- fmt.Errorf("invalid JSON-RPC content length %q", value)
				return
			}
		}
		if length < 0 {
			c.readErr <- fmt.Errorf("JSON-RPC frame missing Content-Length")
			return
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			c.readErr <- err
			return
		}
		var message fixtureRPCMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			c.readErr <- fmt.Errorf("invalid JSON-RPC JSON: %w", err)
			return
		}
		select {
		case c.readPending <- struct{}{}:
		default:
		}
		select {
		case c.messages <- message:
		case <-c.readCancel:
			return
		}
	}
}

func (c *fixtureRawClient) shutdown(ctx context.Context) error {
	if err := c.request(200, "shutdown", nil); err != nil {
		return err
	}
	c.notify("exit", nil)
	c.writeMu.Lock()
	c.closed = true
	err := c.stdin.Close()
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	// StdoutPipe requires its consumer to finish before Wait. In particular,
	// Wait closes the pipe, which can otherwise turn a clean EOF into an
	// os.ErrClosed framing failure in readFrames.
	select {
	case <-c.readDone:
	case <-ctx.Done():
		return fmt.Errorf("fixture stdout did not close after shutdown: %w", ctx.Err())
	}
	select {
	case err := <-c.readErr:
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("fixture stdout was not exclusively framed JSON-RPC: %w", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	c.startWait()
	select {
	case err := <-c.waitDone:
		c.waited = true
		if err != nil {
			return fmt.Errorf("fixture child exit: %w (stderr: %s)", err, c.stderr.String())
		}
	case <-ctx.Done():
		return fmt.Errorf("fixture child did not exit after shutdown: %w", ctx.Err())
	}
	return nil
}

func (c *fixtureRawClient) startWait() {
	c.waitOnce.Do(func() {
		go func() { c.waitDone <- c.cmd.Wait() }()
	})
}

func (c *fixtureRawClient) stop() {
	c.readOnce.Do(func() { close(c.readCancel) })
	c.writeMu.Lock()
	if !c.closed {
		c.closed = true
		_ = c.stdin.Close()
	}
	c.writeMu.Unlock()
	if c.cmd.Process != nil && !c.waited {
		_ = c.cmd.Process.Kill()
		select {
		case <-c.readDone:
			c.startWait()
		case <-time.After(2 * time.Second):
			// Keep Wait ordered after the StdoutPipe reader even during cleanup.
			// Killing the child should close stdout; if it does not, avoid calling
			// Wait against a still-active pipe reader.
		}
		if isClosed(c.readDone) {
			select {
			case <-c.waitDone:
				c.waited = true
			case <-time.After(2 * time.Second):
			}
		}
	}
	select {
	case <-c.stderrEnd:
	case <-time.After(2 * time.Second):
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestNeovimFixtureServer is a test-binary stdio endpoint for the external
// headless Neovim acceptance client. Do not run it through `go test` with the
// fixture enabled; build the package test binary and invoke this test directly.
func TestNeovimFixtureServer(t *testing.T) {
	if os.Getenv("SQLS_NEOVIM_FIXTURE") != "1" {
		t.Skip("set SQLS_NEOVIM_FIXTURE=1 to run the stdio LSP fixture")
	}
	serveNeovimFixture(t)
}

func neovimFixtureCache(variant string) *catalog.Cache {
	nameType := "VARCHAR(80)"
	if variant == "new" {
		nameType = "VARCHAR(100)"
	}
	columns := []*catalog.ColumnDesc{
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "CUSTOMER_ID"}, Type: "INTEGER", Null: "NO", Key: "YES"},
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "NAME"}, Type: nameType, Null: "YES", Key: "NO"},
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "EMAIL"}, Type: "VARCHAR(120)", Null: "YES", Key: "NO"},
	}
	proc := &catalog.ProcedureDesc{Name: "FIND_CUSTOMER", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "P_CUSTOMER_ID", Position: 0, Direction: catalog.ParameterInput, Type: "INTEGER"}}, OutputParameters: []*catalog.ProcedureParameterDesc{{Name: "O_NAME", Position: 0, Direction: catalog.ParameterOutput, Type: "VARCHAR(80)"}}}
	cache := &catalog.Cache{
		DefaultSchema: "", SchemaTables: map[string][]string{"": {"CUSTOMERS"}},
		ColumnsWithParent: catalog.GenColumnMap(columns), ColumnsByID: map[string][]*catalog.ColumnDesc{(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "CUSTOMERS"}).Key(): columns},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{}, PrimaryKeyColumns: map[string]map[string]struct{}{"\tCUSTOMERS": {"CUSTOMER_ID": {}}},
		Catalog: &catalog.CatalogCache{
			Procedures:     map[string]*catalog.ProcedureDesc{"FIND_CUSTOMER": proc},
			ProceduresByID: map[string]*catalog.ProcedureDesc{(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "FIND_CUSTOMER"}).Key(): proc},
			Views:          map[string]*catalog.ViewDesc{}, ViewsByID: map[string]*catalog.ViewDesc{},
			Indexes:     map[string]*catalog.IndexDesc{"PK_CUSTOMERS": {Name: "PK_CUSTOMERS", RelationName: "CUSTOMERS", Columns: []string{"CUSTOMER_ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}},
			IndexesByID: map[string]*catalog.IndexDesc{},
		},
	}
	cache.Catalog.RebuildObjects(strings.ToUpper)
	return cache
}

const (
	neovimFixtureTableDDL         = "CREATE TABLE CUSTOMERS (CUSTOMER_ID INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR(80), EMAIL VARCHAR(120));"
	neovimFixtureProcedureSource  = "BEGIN\n  SELECT NAME FROM CUSTOMERS WHERE CUSTOMER_ID = :P_CUSTOMER_ID INTO :O_NAME;\n  SUSPEND;\nEND"
	neovimFixtureProcedureBuffer  = "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv = 0;\nv = :v + CHAR_LENGTH('😀') + v; -- v\nSELECT 'v' FROM t;\nEND"
	neovimFixtureProcedureSymbols = "CREATE PROCEDURE P AS DECLARE VARIABLE X INTEGER; BEGIN X = 1; END"
)

type neovimFixtureProvider struct {
	variant string
}

func (p *neovimFixtureProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: database.MetadataRelations, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}, Count: 1}, nil
		}},
		{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: cache.ColumnsWithParent, ColumnsByID: cache.ColumnsByID}, Count: 3}, nil
		}},
		{Kind: database.MetadataPrimaryKeys, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			keys := map[string]map[string]struct{}{"\tCUSTOMERS": {"CUSTOMER_ID": {}}}
			return catalog.MetadataPatch{Cache: &catalog.Cache{PrimaryKeyColumns: keys}, Count: 1}, nil
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: cache.Catalog.Procedures, ProceduresByID: cache.Catalog.ProceduresByID}}, Count: 1}, nil
		}},
		{Kind: database.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}, ViewsByID: map[string]*catalog.ViewDesc{}}}}, nil
		}},
		{Kind: database.MetadataIndexes, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Indexes: cache.Catalog.Indexes, IndexesByID: cache.Catalog.IndexesByID}}, Count: 1}, nil
		}},
	}}
}
func (p *neovimFixtureProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	text := neovimFixtureTableDDL
	if p.variant == "new" {
		text = strings.ReplaceAll(text, "VARCHAR(80)", "VARCHAR(100)")
	}
	if req.Object.Kind == catalog.ObjectKindProcedure {
		text = neovimFixtureProcedureSource
	}
	return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataRelations}, Coverage: req.Object.Key() + "\x00fixture-source", Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
		return metadata.Detail{Object: req.Object, Text: text, Provenance: "fixture"}, nil
	}}, nil
}
func (*neovimFixtureProvider) Close() error { return nil }

func (p *neovimFixtureProvider) factory(context.Context) (metadata.Provider, error) {
	return p, nil
}

func serveNeovimFixture(t *testing.T) {
	// testing emits its final PASS line to os.Stdout. Keep the original file
	// handle solely for JSON-RPC and redirect the test harness/log output.
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)
	backend := installStubBackend(t)
	_ = backend
	registerNeovimFixtureDriver()
	notifier := &fixtureRPCNotifier{}
	activeFixtureNotifier.Store(notifier)
	defer activeFixtureNotifier.CompareAndSwap(notifier, nil)
	server := NewServer()
	server.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		notifier.setTarget(cfg.DataSourceName)
		return database.OpenContext(ctx, cfg)
	}
	server.stateMu.Lock()
	server.metadataFactoryOverride = func(cfg *database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		variant := "old"
		if strings.HasSuffix(cfg.DataSourceName, "/new") {
			variant = "new"
		}
		provider := &neovimFixtureProvider{variant: variant}
		return metadata.Key{Engine: "interbase", URL: cfg.DataSourceName}, metadata.Scope{Principal: "fixture"}, provider.factory, true
	}
	server.stateMu.Unlock()
	handler := NewDispatcher(jsonrpc2.HandlerWithError(server.Handle))
	stream := jsonrpc2.NewBufferedStream(fixtureStdio{Reader: os.Stdin, Writer: protocolOut}, jsonrpc2.VSCodeObjectCodec{})
	conn := jsonrpc2.NewConn(context.Background(), stream, handler, jsonrpc2.SetLogger(log.New(os.Stderr, "sqls fixture: ", 0)))
	notifier.setConn(conn)
	<-conn.DisconnectNotify()
	_ = server.Stop()
	<-server.cleanupDone
}

const neovimFixtureDriver dialect.DatabaseDriver = "sqls-neovim-fixture"

var fixtureDriverOnce sync.Once
var activeFixtureNotifier atomic.Pointer[fixtureRPCNotifier]

type fixtureRPCNotifier struct {
	mu     sync.RWMutex
	conn   *jsonrpc2.Conn
	target string
}

func (n *fixtureRPCNotifier) setConn(conn *jsonrpc2.Conn) {
	n.mu.Lock()
	n.conn = conn
	n.mu.Unlock()
}

func (n *fixtureRPCNotifier) setTarget(target string) {
	n.mu.Lock()
	n.target = target
	n.mu.Unlock()
}

func (n *fixtureRPCNotifier) queryEntered(query string) error {
	n.mu.RLock()
	conn, target := n.conn, n.target
	n.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("fixture LSP connection is not initialized")
	}
	return conn.Notify(context.Background(), "sqls/fixtureQueryEntered", map[string]string{
		"marker": "SQLS_FIXTURE_WAIT", "sql": query, "target": target,
	})
}

func registerNeovimFixtureDriver() {
	fixtureDriverOnce.Do(func() {
		database.RegisterOpen(neovimFixtureDriver, func(*database.DBConfig) (*database.DBConnection, error) {
			db, err := sql.Open("sqls-handler-stub", "")
			if err != nil {
				return nil, err
			}
			return &database.DBConnection{Conn: db, Driver: dialect.DatabaseDriverInterBase}, nil
		})
		database.RegisterFactory(neovimFixtureDriver, func(db *sql.DB) database.DBRepository {
			backend := activeStubBackend()
			if backend == nil {
				backend = &stubBackend{gates: map[string]*stubGate{}}
			}
			return &neovimFixtureRepository{stubRepository: &stubRepository{MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository), backend: backend, db: db}, notifier: activeFixtureNotifier.Load()}
		})
	})
}

type fixtureStdio struct {
	io.Reader
	io.Writer
}

func (fixtureStdio) Close() error { return nil }

type neovimFixtureRepository struct {
	*stubRepository
	notifier *fixtureRPCNotifier
}

func (r *neovimFixtureRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	if strings.Contains(query, "SQLS_FIXTURE_WAIT") {
		r.backend.recordQuery(r.db, query)
		if r.notifier != nil {
			if err := r.notifier.queryEntered(query); err != nil {
				return nil, err
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.stubRepository.Query(ctx, query)
}

func (*neovimFixtureRepository) ExplainPlan(context.Context, string) (string, error) {
	return "PLAN (CUSTOMERS NATURAL)", nil
}
