package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestDecodeCompletionAcceptsListArrayAndNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"list", `{"isIncomplete":true,"items":[{"label":"TABLE_A"}]}`, []string{"TABLE_A"}},
		{"array", `[{"label":"TABLE_A"}]`, []string{"TABLE_A"}},
		{"null", `null`, nil},
		{"empty", ``, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := decodeCompletion(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != len(tc.want) {
				t.Fatalf("got %d items, want %d", len(items), len(tc.want))
			}
			for i := range items {
				if items[i].Label != tc.want[i] {
					t.Fatalf("label[%d]=%q, want %q", i, items[i].Label, tc.want[i])
				}
			}
		})
	}
}

func TestAggregateExcludesFailedRunsFromPercentilesButCountsThem(t *testing.T) {
	results := []runResult{
		{Outcome: "success", SettledMS: float64Ptr(10)},
		{Outcome: "success", SettledMS: float64Ptr(30)},
		{Outcome: "timeout"},
		{Outcome: "degraded", SettledMS: float64Ptr(100)},
	}
	got := aggregate(results)
	if got.SuccessfulRuns != 2 || got.FailedRuns != 2 {
		t.Fatalf("counts = %d successful / %d failed", got.SuccessfulRuns, got.FailedRuns)
	}
	if got.SettledMS.P50 == nil || *got.SettledMS.P50 != 10 || got.SettledMS.P95 == nil || *got.SettledMS.P95 != 30 {
		t.Fatalf("successful timing percentiles = %+v", got.SettledMS)
	}
}

func TestAggregateReportsOnlyLoadingCompletionLatency(t *testing.T) {
	got := aggregate([]runResult{
		{Outcome: "success", CompletionLatencyMS: []float64{4, 8}},
		{Outcome: "success", CompletionLatencyMS: []float64{6}},
		{Outcome: "timeout", CompletionLatencyMS: []float64{1000}},
	})
	if got.LoadingCompletionLatencyMS.P50 == nil || *got.LoadingCompletionLatencyMS.P50 != 6 ||
		got.LoadingCompletionLatencyMS.P95 == nil || *got.LoadingCompletionLatencyMS.P95 != 8 {
		t.Fatalf("loading latency summary = %+v", got.LoadingCompletionLatencyMS)
	}
}

// TestHelperProcess is the benchmark server fixture. The protocol deliberately
// writes unrelated stderr noise; benchmark reports must never retain it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SQLS_BENCH_HELPER") != "1" {
		return
	}
	mode := os.Getenv("SQLS_BENCH_MODE")
	if pidFile := os.Getenv("SQLS_BENCH_PIDFILE"); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0600)
	}
	if mode == "stall" {
		select {}
	}
	if mode == "noise" {
		_, _ = os.Stderr.WriteString("password=do-not-report\n")
	}
	serveHelper(os.Stdin, os.Stdout, mode)
	os.Exit(0)
}

func TestStdioRunnerHelperChild(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess protocol test")
	}
	server := helperServer(t, "success")
	result := runOne(context.Background(), server, "unused-config.yml", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", result.Outcome)
	}
	if result.ProtocolReadyMS == nil || result.BasicReadyMS == nil || result.SettledMS == nil {
		t.Fatalf("missing successful milestones: %+v", result)
	}
	if result.Observer != "status" {
		t.Fatalf("observer = %q, want status", result.Observer)
	}
	if result.AttachReadyMS == nil || result.RelationReadyMS == nil || len(result.CompletionLatencyMS) != 0 {
		t.Fatalf("status readiness or post-settlement latency isolation failed: %+v", result)
	}
}

func TestStdioRunnerFailedCatalogIsDegraded(t *testing.T) {
	server := helperServer(t, "failed-catalog")
	result := runOne(context.Background(), server, "unused", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "degraded" {
		t.Fatalf("outcome = %q, want degraded", result.Outcome)
	}
}

func TestReadyLoaderWithoutSentinelIsNotSuccess(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "missing-table"), "unused", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "degraded" {
		t.Fatalf("settled loader without table sentinel outcome = %q, want degraded", result.Outcome)
	}
}

func TestKnownDegradedSettlementWithoutSentinelTerminatesDegraded(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "degraded-missing-table"), "unused", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "degraded" || result.SettledMS == nil {
		t.Fatalf("degraded settlement did not terminate immediately: %+v", result)
	}
}

func TestStdioRunnerStallKillsAndReapsChildWithUnsetMetrics(t *testing.T) {
	server, pidFile := helperServerWithPID(t, "stall")
	started := time.Now()
	result := runOne(context.Background(), server, "unused", testProbe(), "cold-process", 80*time.Millisecond)
	if time.Since(started) > 2*time.Second {
		t.Fatal("watchdog failed to terminate/reap stalled child")
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("helper PID file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("helper PID: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("helper process %d was not reaped; kill(pid, 0) = %v", pid, err)
	}
	if result.Outcome != "timeout" {
		t.Fatalf("outcome=%q, want timeout", result.Outcome)
	}
	if result.ProtocolReadyMS != nil || result.AttachReadyMS != nil || result.RelationReadyMS != nil || result.BasicReadyMS != nil || result.SettledMS != nil {
		t.Fatalf("timeout retained partial readiness metrics: %+v", result)
	}
}

func TestStdioRunnerDoesNotEchoStderrNoise(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "noise"), "unused", testProbe(), "cold-process", 2*time.Second)
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "do-not-report") {
		t.Fatalf("stderr leaked in result: %s", encoded)
	}
}

func TestStdioRunnerWithoutStatusUsesLegacyLogs(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "no-status"), "unused", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "success" || result.Observer != "legacy-log" {
		t.Fatalf("result = %+v", result)
	}
}

func TestStdioRunnerDelayedInitializedWorkAndOutOfOrderReplies(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "delayed"), "unused", testProbe(), "cold-process", 2*time.Second)
	if result.Outcome != "success" || result.ProtocolReadyMS == nil || *result.ProtocolReadyMS < 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.tableLatencyMS) == 0 || len(result.columnLatencyMS) == 0 || result.columnLatencyMS[0] >= result.tableLatencyMS[0] {
		t.Fatalf("out-of-order response latencies were not independently attributed: table=%v column=%v", result.tableLatencyMS, result.columnLatencyMS)
	}
	if len(result.CompletionLatencyMS) < 2 {
		t.Fatalf("loading-phase samples were not recorded: %v", result.CompletionLatencyMS)
	}
}

func TestRefreshWarmupIgnoresLegacyLogsForStatusObserver(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "candidate-log-contamination"), "unused", testProbe(), "refresh", 2*time.Second)
	if result.Outcome != "success" || result.Observer != "status" {
		t.Fatalf("candidate refresh result = %+v", result)
	}
}

func TestRefreshWarmupDegradedStopsBeforeSwitch(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "failed-catalog"), "unused", testProbe(), "refresh", 2*time.Second)
	if result.Outcome != "degraded" {
		t.Fatalf("degraded refresh warm-up outcome = %q, want degraded", result.Outcome)
	}
}

func TestRefreshScenarioMeasuresNewGeneration(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "refresh"), "unused", testProbe(), "refresh", 2*time.Second)
	if result.Outcome != "success" || result.ProtocolReadyMS != nil || result.SettledMS == nil {
		t.Fatalf("refresh result = %+v", result)
	}
}

func TestRefreshBaselineUsesFreshLegacyEvents(t *testing.T) {
	result := runOne(context.Background(), helperServer(t, "no-status"), "unused", testProbe(), "refresh", 2*time.Second)
	if result.Outcome != "success" || result.Observer != "legacy-log" || result.AttachReadyMS != nil {
		t.Fatalf("legacy refresh result = %+v", result)
	}
}

func testProbe() probe {
	return probe{Text: "select * from T;", ExpectedTable: "TABLE_SENTINEL", ExpectedColumn: "COLUMN_SENTINEL", TablePosition: lsp.Position{Line: 0, Character: 8}, ColumnPosition: lsp.Position{Line: 0, Character: 15}}
}

func helperServer(t *testing.T, mode string) string {
	path, _ := helperServerWithPID(t, mode)
	return path
}

func helperServerWithPID(t *testing.T, mode string) (string, string) {
	t.Helper()
	path := os.Args[0]
	// A tiny executable wrapper reinvokes this test binary in helper mode.
	dir := t.TempDir()
	script := dir + "/server"
	pidFile := filepath.Join(dir, "child.pid")
	content := "#!/bin/sh\nSQLS_BENCH_HELPER=1 SQLS_BENCH_MODE=" + mode + " SQLS_BENCH_PIDFILE=" + shellQuote(pidFile) + " exec " + shellQuote(path) + " -test.run=^TestHelperProcess$\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	return script, pidFile
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

type fixtureHandler struct {
	mu         sync.Mutex
	mode       string
	readyAt    time.Time
	generation uint64
	tableSeen  bool
	columnSeen bool
}

func (h *fixtureHandler) Handle(ctx context.Context, c *jsonrpc2.Conn, req *jsonrpc2.Request) {
	switch req.Method {
	case "initialize":
		h.mu.Lock()
		h.generation = 1
		h.mu.Unlock()
		caps := map[string]interface{}{}
		if h.mode != "no-status" {
			caps["executeCommandProvider"] = map[string]interface{}{"commands": []string{"sqls.showMetadataStatus"}}
		}
		_ = c.Reply(ctx, req.ID, map[string]interface{}{"capabilities": caps})
	case "initialized":
		h.mu.Lock()
		if h.mode == "delayed" {
			h.readyAt = time.Now().Add(120 * time.Millisecond)
		} else if h.mode == "candidate-log-contamination" {
			h.readyAt = time.Now().Add(180 * time.Millisecond)
		} else {
			h.readyAt = time.Now()
		}
		mode := h.mode
		h.mu.Unlock()
		if mode == "no-status" || mode == "candidate-log-contamination" {
			_, _ = fmt.Fprintln(os.Stderr, "db worker: Update db cache primary complete")
			_, _ = fmt.Fprintln(os.Stderr, "db worker: Update catalog cache complete")
		}
	case "textDocument/completion":
		var params struct {
			Position lsp.Position `json:"position"`
		}
		if req.Params != nil {
			_ = json.Unmarshal(*req.Params, &params)
		}
		h.mu.Lock()
		mode := h.mode
		readyAt := h.readyAt
		h.mu.Unlock()
		if params.Position.Character == 8 {
			time.Sleep(120 * time.Millisecond)
			if (mode == "candidate-log-contamination" || mode == "failed-catalog") && time.Now().Before(readyAt) {
				_ = c.Reply(ctx, req.ID, []map[string]string{{"label": "OTHER"}})
				return
			}
			if mode == "missing-table" || mode == "degraded-missing-table" {
				_ = c.Reply(ctx, req.ID, []map[string]string{{"label": "OTHER"}})
				return
			}
			if mode == "no-status" {
				_ = c.Reply(ctx, req.ID, []map[string]string{{"label": "TABLE_SENTINEL"}})
			} else {
				_ = c.Reply(ctx, req.ID, map[string]interface{}{"isIncomplete": true, "items": []map[string]string{{"label": "TABLE_SENTINEL"}}})
			}
		} else {
			time.Sleep(5 * time.Millisecond)
			if (mode == "candidate-log-contamination" || mode == "failed-catalog") && time.Now().Before(readyAt) {
				_ = c.Reply(ctx, req.ID, []map[string]string{{"label": "OTHER"}})
				return
			}
			_ = c.Reply(ctx, req.ID, []map[string]string{{"label": "COLUMN_SENTINEL"}})
			h.mu.Lock()
			h.columnSeen = true
			h.mu.Unlock()
		}
		if params.Position.Character == 8 {
			h.mu.Lock()
			h.tableSeen = true
			h.mu.Unlock()
		}
	case "workspace/executeCommand":
		var params struct {
			Command string `json:"command"`
		}
		if req.Params != nil {
			_ = json.Unmarshal(*req.Params, &params)
		}
		if params.Command == "switchConnections" {
			var args struct {
				Arguments []interface{} `json:"arguments"`
			}
			if req.Params != nil {
				_ = json.Unmarshal(*req.Params, &args)
			}
			h.mu.Lock()
			if len(args.Arguments) != 1 || args.Arguments[0] != "1" {
				h.mu.Unlock()
				_ = c.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "expected connection 1"})
				return
			}
			if h.mode == "failed-catalog" || h.mode == "degraded-missing-table" {
				h.mu.Unlock()
				_ = c.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidRequest, Message: "degraded warm-up must not refresh"})
				return
			}
			if h.mode == "candidate-log-contamination" && (time.Now().Before(h.readyAt) || !h.tableSeen || !h.columnSeen) {
				h.mu.Unlock()
				_ = c.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidRequest, Message: "refresh began before warmup ready"})
				return
			}
			h.generation++
			h.readyAt = time.Now()
			mode := h.mode
			h.mu.Unlock()
			if mode == "no-status" {
				_, _ = fmt.Fprintln(os.Stderr, "db worker: Update db cache primary complete")
				_, _ = fmt.Fprintln(os.Stderr, "db worker: Update catalog cache complete")
			}
			_ = c.Reply(ctx, req.ID, nil)
			return
		}
		if h.mode == "no-status" {
			_ = c.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "unsupported"})
			return
		}
		h.mu.Lock()
		ready := !h.readyAt.IsZero() && time.Now().After(h.readyAt)
		generation := h.generation
		mode := h.mode
		h.mu.Unlock()
		state := "connecting"
		categories := []map[string]interface{}{}
		settled, degraded := false, false
		if ready {
			state = "ready"
			settled = true
			degraded = mode == "failed-catalog" || mode == "degraded-missing-table"
			categories = append(categories, map[string]interface{}{"kind": "relations", "state": "ready", "count": 1, "durationMs": 1}, map[string]interface{}{"kind": "columns", "state": "ready", "count": 1, "durationMs": 1})
		}
		_ = c.Reply(ctx, req.ID, map[string]interface{}{"generation": generation, "revision": generation, "connectionState": state, "settled": settled, "degraded": degraded, "categories": categories})
	case "shutdown":
		_ = c.Reply(ctx, req.ID, nil)
	}
}

func serveHelper(in io.Reader, out io.Writer, mode string) {
	stream := jsonrpc2.NewBufferedStream(&stdioRWC{Reader: in, Writer: out}, jsonrpc2.VSCodeObjectCodec{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := jsonrpc2.NewConn(ctx, stream, jsonrpc2.AsyncHandler(&fixtureHandler{mode: mode}))
	<-c.DisconnectNotify()
}

type stdioRWC struct {
	io.Reader
	io.Writer
}

func (*stdioRWC) Close() error { return nil }
