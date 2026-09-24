// Command metadata-benchmark measures readiness using the same stdio LSP
// protocol used by editor clients.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/lsp"
)

const pollInterval = 100 * time.Millisecond
const maxLoadingSamplesPerSurface = 5

var errDegradedWarmup = errors.New("refresh warm-up degraded")

type milestone struct {
	line string
	at   time.Time
}

type probe struct {
	Text           string       `json:"text"`
	TablePosition  lsp.Position `json:"tablePosition"`
	ColumnPosition lsp.Position `json:"columnPosition"`
	ExpectedTable  string       `json:"expectedTable"`
	ExpectedColumn string       `json:"expectedColumn"`
}

type runResult struct {
	Outcome             string    `json:"outcome"`
	ProtocolReadyMS     *float64  `json:"protocolReadyMs"`
	AttachReadyMS       *float64  `json:"attachReadyMs"`
	RelationReadyMS     *float64  `json:"relationReadyMs"`
	BasicReadyMS        *float64  `json:"basicReadyMs"`
	SettledMS           *float64  `json:"settledMs"`
	CompletionLatencyMS []float64 `json:"completionLatencyMs"`
	Observer            string    `json:"observer"`
	tableLatencyMS      []float64 `json:"-"`
	columnLatencyMS     []float64 `json:"-"`
}

type percentile struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}
type completionResponse struct {
	table   bool
	items   []lsp.CompletionItem
	latency float64
	valid   bool
	sampled bool
}
type report struct {
	aggregateReport
	GeneratedAt       string `json:"generatedAt"`
	RunnerVersion     string `json:"runnerVersion"`
	ServerVersion     string `json:"serverVersion"`
	GoVersion         string `json:"goVersion"`
	Platform          string `json:"platform"`
	RunCount          int    `json:"runCount"`
	Scenario          string `json:"scenario"`
	PollingIntervalMS int    `json:"pollingIntervalMs"`
	ProbeSHA256       string `json:"probeSha256"`
}

func main() {
	var serverPath, configPath, probePath, scenario, output string
	var runs int
	var timeout time.Duration
	flag.StringVar(&serverPath, "server", "", "sqls binary (required)")
	flag.StringVar(&configPath, "config", "", "existing sqls YAML config (required)")
	flag.StringVar(&probePath, "probe", "", "JSON probe document (required)")
	flag.IntVar(&runs, "runs", 10, "number of measured runs")
	flag.StringVar(&scenario, "scenario", "cold-process", "cold-process or refresh")
	flag.StringVar(&output, "output", "", "JSON report path (required)")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "per-run watchdog")
	flag.Parse()
	if serverPath == "" || configPath == "" || probePath == "" || output == "" || runs < 1 || timeout <= 0 || (scenario != "cold-process" && scenario != "refresh") {
		fmt.Fprintln(os.Stderr, "invalid arguments: -server, -config, -probe, -output required; runs >= 1; scenario cold-process|refresh; timeout > 0")
		os.Exit(2)
	}
	if err := runCLI(serverPath, configPath, probePath, scenario, output, runs, timeout); err != nil {
		fmt.Fprintln(os.Stderr, "metadata benchmark failed")
		os.Exit(1)
	}
}

func runCLI(serverPath, configPath, probePath, scenario, output string, runs int, timeout time.Duration) error {
	probeBytes, err := os.ReadFile(probePath)
	if err != nil {
		return fmt.Errorf("read probe: %w", err)
	}
	var p probe
	if err := json.Unmarshal(probeBytes, &p); err != nil {
		return fmt.Errorf("decode probe: %w", err)
	}
	if p.Text == "" || p.ExpectedTable == "" || p.ExpectedColumn == "" {
		return errors.New("probe text and expected labels are required")
	}
	serverVersion := "unknown"
	versionCtx, cancelVersion := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelVersion()
	if out, err := exec.CommandContext(versionCtx, serverPath, "-version").Output(); err == nil {
		serverVersion = strings.TrimSpace(string(out))
	}
	results := make([]runResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runOne(context.Background(), serverPath, configPath, p, scenario, timeout))
	}
	rep := report{aggregateReport: aggregate(results)}
	rep.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	rep.RunnerVersion, rep.ServerVersion, rep.GoVersion = "1", serverVersion, runtime.Version()
	rep.Platform, rep.RunCount, rep.Scenario = runtime.GOOS+"/"+runtime.GOARCH, runs, scenario
	rep.PollingIntervalMS, rep.ProbeSHA256 = int(pollInterval.Milliseconds()), fmt.Sprintf("%x", sha256.Sum256(probeBytes))
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(output, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func runOne(parent context.Context, serverPath, configPath string, p probe, scenario string, timeout time.Duration) runResult {
	result := runResult{Outcome: "transport-error", CompletionLatencyMS: []float64{}}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	cmd := exec.CommandContext(ctx, serverPath, "-config", configPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return result
	}
	if err := cmd.Start(); err != nil {
		return result
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	logCh := make(chan milestone, 16)
	go captureMilestones(stderr, logCh)
	stream := &stdioStream{r: stdout, w: stdin}
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	conn := jsonrpc2.NewConn(connCtx, jsonrpc2.NewBufferedStream(stream, jsonrpc2.VSCodeObjectCodec{}), nil, jsonrpc2.SetLogger(log.New(io.Discard, "", 0)))
	defer func() {
		_ = stdin.Close()
		_ = stdout.Close()
		connCancel()
		select {
		case <-waitCh:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	}()

	statusSupported := false
	var priorGeneration uint64
	if scenario == "refresh" {
		var err error
		statusSupported, _, err = initializeAndOpen(ctx, conn, p, "file:///metadata-benchmark.sql", started)
		if err != nil {
			return timeoutOrError(ctx, result)
		}
		result.Observer = observerName(statusSupported)
		priorGeneration, err = warmupRefresh(ctx, conn, p, logCh, result.Observer)
		if err != nil {
			if errors.Is(err, errDegradedWarmup) {
				result.Outcome = "degraded"
				sendShutdown(ctx, conn)
				return result
			}
			return timeoutOrError(ctx, result)
		}
		for {
			select {
			case <-logCh:
			default:
				goto logsDrained
			}
		}
	logsDrained:
		started = time.Now()
		if err := conn.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: "switchConnections", Arguments: []interface{}{"1"}}, nil); err != nil {
			return timeoutOrError(ctx, result)
		}
	} else {
		var err error
		var protocolReady *float64
		statusSupported, protocolReady, err = initializeAndOpen(ctx, conn, p, "file:///metadata-benchmark.sql", started)
		if err != nil {
			return timeoutOrError(ctx, result)
		}
		result.ProtocolReadyMS = protocolReady
	}
	result.Observer = observerName(statusSupported)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var tableReady, colReady bool
	var settled, degraded bool
	var tableInFlight, colInFlight bool
	var tableLoadingSamples, colLoadingSamples int
	var tableValidationIssued, colValidationIssued bool
	var tableCompleted, colCompleted bool
	responses := make(chan completionResponse, 4)
	for {
		select {
		case <-ctx.Done():
			return timeoutOrError(ctx, result)
		case event, ok := <-logCh:
			if !ok {
				logCh = nil
				continue
			}
			if result.Observer == "legacy-log" && strings.Contains(event.line, "Update db cache primary complete") && result.RelationReadyMS == nil {
				result.RelationReadyMS = elapsedAt(started, event.at)
			}
			if result.Observer == "legacy-log" && strings.Contains(event.line, "Update catalog cache complete") {
				settled = true
				if result.SettledMS == nil {
					result.SettledMS = elapsedAt(started, event.at)
				}
			}
		case response := <-responses:
			if response.table {
				tableInFlight = false
				tableCompleted = true
			} else {
				colInFlight = false
				colCompleted = true
			}
			if response.valid && response.sampled {
				result.CompletionLatencyMS = append(result.CompletionLatencyMS, response.latency)
				if response.table {
					result.tableLatencyMS = append(result.tableLatencyMS, response.latency)
				} else {
					result.columnLatencyMS = append(result.columnLatencyMS, response.latency)
				}
			}
			if containsLabel(response.items, func() string {
				if response.table {
					return p.ExpectedTable
				}
				return p.ExpectedColumn
			}()) {
				if response.table {
					tableReady = true
				} else {
					colReady = true
				}
			}
			if tableReady && colReady && result.BasicReadyMS == nil {
				result.BasicReadyMS = elapsed(started)
			}
		case <-ticker.C:
			if result.Observer == "status" {
				var status lsp.MetadataStatusResult
				err := conn.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: "sqls.showMetadataStatus"}, &status)
				if err == nil {
					if scenario == "refresh" && status.Generation <= priorGeneration {
						continue
					}
					if status.ConnectionState == "ready" && result.AttachReadyMS == nil {
						result.AttachReadyMS = elapsed(started)
					}
					for _, category := range status.Categories {
						if category.Kind == "relations" && category.State == "ready" && result.RelationReadyMS == nil {
							result.RelationReadyMS = elapsed(started)
						}
					}
					wasSettled := settled
					settled, degraded = status.Settled, status.Degraded
					if settled && !wasSettled && result.SettledMS == nil {
						result.SettledMS = elapsed(started)
					}
				}
			}
			if tableReady && colReady && result.BasicReadyMS == nil {
				result.BasicReadyMS = elapsed(started)
			}
			if result.BasicReadyMS != nil && settled && result.SettledMS == nil {
				result.SettledMS = elapsed(started)
			}
			if !tableInFlight && ((!settled && tableLoadingSamples < maxLoadingSamplesPerSurface) || (settled && !tableReady && !tableValidationIssued)) {
				loading := !settled
				if loading {
					tableLoadingSamples++
				} else {
					tableValidationIssued = true
				}
				tableInFlight = dispatchCompletion(ctx, conn, p.TablePosition, true, loading, responses)
			}
			if !colInFlight && ((!settled && colLoadingSamples < maxLoadingSamplesPerSurface) || (settled && !colReady && !colValidationIssued)) {
				loading := !settled
				if loading {
					colLoadingSamples++
				} else {
					colValidationIssued = true
				}
				colInFlight = dispatchCompletion(ctx, conn, p.ColumnPosition, false, loading, responses)
			}
			if tableReady && colReady && result.BasicReadyMS == nil {
				result.BasicReadyMS = elapsed(started)
			}
			if result.BasicReadyMS != nil && settled && result.SettledMS == nil {
				result.SettledMS = elapsed(started)
			}
			if settled && degraded {
				if result.SettledMS == nil {
					result.SettledMS = elapsed(started)
				}
				result.Outcome = "degraded"
				sendShutdown(ctx, conn)
				return result
			}
			if settled && result.BasicReadyMS != nil {
				if result.SettledMS == nil {
					result.SettledMS = elapsed(started)
				}
				if !tableReady || !colReady {
					result.Outcome = "degraded"
				} else {
					result.Outcome = "success"
				}
				sendShutdown(ctx, conn)
				return result
			}
			if settled && !degraded && tableCompleted && colCompleted && !tableInFlight && !colInFlight && (!tableReady || !colReady) {
				result.Outcome = "degraded"
				sendShutdown(ctx, conn)
				return result
			}
		}
	}
}

func initializeAndOpen(ctx context.Context, conn *jsonrpc2.Conn, p probe, uri string, started time.Time) (bool, *float64, error) {
	var initialized map[string]interface{}
	if err := conn.Call(ctx, "initialize", map[string]interface{}{"processId": nil, "rootUri": nil, "capabilities": map[string]interface{}{}}, &initialized); err != nil {
		return false, nil, err
	}
	status := supportsStatus(initialized)
	protocolReady := elapsed(started)
	if err := conn.Notify(ctx, "initialized", map[string]interface{}{}); err != nil {
		return status, protocolReady, err
	}
	if err := conn.Notify(ctx, "textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri, "languageId": "sql", "version": 1, "text": p.Text}}); err != nil {
		return status, protocolReady, err
	}
	return status, protocolReady, nil
}
func supportsStatus(result map[string]interface{}) bool {
	capabilities, _ := result["capabilities"].(map[string]interface{})
	commands, _ := capabilities["executeCommandProvider"].(map[string]interface{})
	list, _ := commands["commands"].([]interface{})
	for _, item := range list {
		if item == "sqls.showMetadataStatus" {
			return true
		}
	}
	return false
}
func completionParams(pos lsp.Position) map[string]interface{} {
	return map[string]interface{}{"textDocument": map[string]interface{}{"uri": "file:///metadata-benchmark.sql"}, "position": pos, "context": map[string]interface{}{"triggerKind": 1}}
}
func sendShutdown(ctx context.Context, conn *jsonrpc2.Conn) {
	_ = conn.Call(ctx, "shutdown", nil, nil)
	_ = conn.Notify(ctx, "exit", nil)
}
func observerName(statusSupported bool) string {
	if statusSupported {
		return "status"
	}
	return "legacy-log"
}
func dispatchCompletion(ctx context.Context, conn *jsonrpc2.Conn, pos lsp.Position, table, loading bool, responses chan<- completionResponse) bool {
	started := time.Now()
	waiter, err := conn.DispatchCall(ctx, "textDocument/completion", completionParams(pos))
	if err != nil {
		return false
	}
	go func() {
		var raw json.RawMessage
		err := waiter.Wait(ctx, &raw)
		items, decodeErr := decodeCompletion(raw)
		response := completionResponse{table: table, items: items, valid: err == nil && decodeErr == nil, sampled: loading}
		if loading && response.valid {
			response.latency = float64(time.Since(started)) / float64(time.Millisecond)
		}
		select {
		case responses <- response:
		case <-ctx.Done():
		}
	}()
	return true
}
func timeoutOrError(ctx context.Context, result runResult) runResult {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Outcome = "timeout"
	}
	return result
}
func elapsed(start time.Time) *float64 {
	return elapsedAt(start, time.Now())
}
func elapsedAt(start, end time.Time) *float64 {
	v := float64(end.Sub(start)) / float64(time.Millisecond)
	return &v
}
func float64Ptr(v float64) *float64 { return &v }
func containsLabel(items []lsp.CompletionItem, label string) bool {
	for _, item := range items {
		if item.Label == label {
			return true
		}
	}
	return false
}
func decodeCompletion(raw json.RawMessage) ([]lsp.CompletionItem, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '[' {
		var items []lsp.CompletionItem
		err := json.Unmarshal(raw, &items)
		return items, err
	}
	var list lsp.CompletionList
	err := json.Unmarshal(raw, &list)
	return list.Items, err
}

func warmupRefresh(ctx context.Context, conn *jsonrpc2.Conn, p probe, logs <-chan milestone, observer string) (uint64, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	responses := make(chan completionResponse, 4)
	var tableReady, colReady, tableInFlight, colInFlight, settled, degraded bool
	var generation uint64
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case event, ok := <-logs:
			if !ok {
				logs = nil
				continue
			}
			if observer == "legacy-log" && strings.Contains(event.line, "Update catalog cache complete") {
				settled = true
			}
		case response := <-responses:
			if response.table {
				tableInFlight = false
				tableReady = tableReady || containsLabel(response.items, p.ExpectedTable)
			} else {
				colInFlight = false
				colReady = colReady || containsLabel(response.items, p.ExpectedColumn)
			}
		case <-ticker.C:
			if observer == "status" {
				var status lsp.MetadataStatusResult
				if err := conn.Call(ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{Command: "sqls.showMetadataStatus"}, &status); err == nil {
					generation = status.Generation
					settled, degraded = status.Settled, status.Degraded
				}
			}
			if settled && degraded {
				return generation, errDegradedWarmup
			}
			if settled && tableReady && colReady {
				return generation, nil
			}
			if !tableReady && !tableInFlight {
				tableInFlight = dispatchCompletion(ctx, conn, p.TablePosition, true, false, responses)
			}
			if !colReady && !colInFlight {
				colInFlight = dispatchCompletion(ctx, conn, p.ColumnPosition, false, false, responses)
			}
		}
	}
}
func captureMilestones(r io.Reader, out chan<- milestone) {
	defer close(out)
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for scan.Scan() {
		line := scan.Text()
		if strings.Contains(line, "Update db cache primary complete") || strings.Contains(line, "Update catalog cache complete") || strings.Contains(line, "metadata generation settled") {
			event := milestone{line: line, at: time.Now()}
			select {
			case out <- event:
			default:
			}
		}
	}
}

type stdioStream struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (s *stdioStream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *stdioStream) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *stdioStream) Close() error                { e := s.w.Close(); _ = s.r.Close(); return e }

type aggregateReport struct {
	SuccessfulRuns             int            `json:"successfulRuns"`
	FailedRuns                 int            `json:"failedRuns"`
	Outcomes                   map[string]int `json:"outcomes"`
	ProtocolReadyMS            percentile     `json:"protocolReadyMs"`
	AttachReadyMS              percentile     `json:"attachReadyMs"`
	RelationReadyMS            percentile     `json:"relationReadyMs"`
	BasicReadyMS               percentile     `json:"basicReadyMs"`
	SettledMS                  percentile     `json:"settledMs"`
	LoadingCompletionLatencyMS percentile     `json:"loadingCompletionLatencyMs"`
	Runs                       []runResult    `json:"runs"`
}

func aggregate(results []runResult) aggregateReport {
	all := aggregateReport{Outcomes: map[string]int{}, Runs: results}
	good := make([]runResult, 0, len(results))
	for _, r := range results {
		all.Outcomes[r.Outcome]++
		if r.Outcome == "success" {
			all.SuccessfulRuns++
			good = append(good, r)
		} else {
			all.FailedRuns++
		}
	}
	all.ProtocolReadyMS = percentiles(good, func(r runResult) *float64 { return r.ProtocolReadyMS })
	all.AttachReadyMS = percentiles(good, func(r runResult) *float64 { return r.AttachReadyMS })
	all.RelationReadyMS = percentiles(good, func(r runResult) *float64 { return r.RelationReadyMS })
	all.BasicReadyMS = percentiles(good, func(r runResult) *float64 { return r.BasicReadyMS })
	all.SettledMS = percentiles(good, func(r runResult) *float64 { return r.SettledMS })
	var loadingLatencies []float64
	for _, run := range good {
		loadingLatencies = append(loadingLatencies, run.CompletionLatencyMS...)
	}
	all.LoadingCompletionLatencyMS = percentileValues(loadingLatencies)
	return all
}
func percentiles(rs []runResult, field func(runResult) *float64) percentile {
	values := []float64{}
	for _, r := range rs {
		if v := field(r); v != nil {
			values = append(values, *v)
		}
	}
	return percentileValues(values)
}
func percentileValues(values []float64) percentile {
	if len(values) == 0 {
		return percentile{}
	}
	sort.Float64s(values)
	p50 := values[(len(values)-1)/2]
	p95 := values[int(math.Ceil(.95*float64(len(values))))-1]
	return percentile{&p50, &p95}
}
