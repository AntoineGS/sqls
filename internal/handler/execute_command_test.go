package handler

import (
	"bytes"
	"strings"
	"testing"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/sqls-server/sqls/internal/lsp"
)

func Test_executeQuery(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))

	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if !strings.Contains(got, "42") {
		t.Errorf("query result = %q, want it to contain the row value 42", got)
	}
	if !strings.Contains(got, "1 rows in set") {
		t.Errorf("query result = %q, want the row-count footer", got)
	}
	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("repository served %d queries, want 1", len(queries))
	}
}

func TestQueryRendersPartialResultBeforeReportingError(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT fail_fetch FROM t;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rows that preceded the failure", got)
	}
	if !strings.Contains(got, "2 rows in set (incomplete)") {
		t.Errorf("result = %q, want the incomplete row-count footer", got)
	}
	if !strings.Contains(got, "Fetch failed: interbase: BLOB result exceeds the materialization limit") {
		t.Errorf("result = %q, want the driver's error text passed through verbatim", got)
	}
}

func TestBlobLimitHintOnlyWithBlobColumn(t *testing.T) {
	const hintFragment = "larger than the driver's 64 MiB"

	for _, tt := range []struct {
		name     string
		text     string
		wantHint bool
	}{
		{name: "blob column", text: "SELECT fail_blob_fetch FROM t;", wantHint: true},
		{name: "no blob column", text: "SELECT fail_fetch FROM t;", wantHint: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.setup(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()

			installStubBackend(t)
			tx.addWorkspaceConfig(t, stubConnections("primary"))
			tx.textDocumentDidOpen(t, testFileURI, tt.text)

			var got string
			if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
				Command:   CommandExecuteQuery,
				Arguments: []interface{}{testFileURI},
			}, &got); err != nil {
				t.Fatal("conn.Call workspace/executeCommand:", err)
			}

			if strings.Contains(got, hintFragment) != tt.wantHint {
				t.Errorf("result = %q, want BLOB hint present = %v", got, tt.wantHint)
			}
			// The failure itself is reported either way; only the advice is
			// conditional, so the advice is never wrong.
			if !strings.Contains(got, "Fetch failed:") {
				t.Errorf("result = %q, want the fetch failure reported", got)
			}
		})
	}
}

func TestQuerySucceedsWithCompleteFooter(t *testing.T) {
	// Regression guard for the footer wording: a complete result must keep the
	// exact "%d rows in set" text the existing pane and tests rely on.
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}
	if !strings.Contains(got, "1 rows in set") {
		t.Errorf("result = %q, want the complete row-count footer", got)
	}
	if strings.Contains(got, "(incomplete)") {
		t.Errorf("result = %q, want no incomplete marker on a complete result", got)
	}
	if strings.Contains(got, "Fetch failed:") {
		t.Errorf("result = %q, want no fetch failure on a complete result", got)
	}
}

func Test_queryResultHeaderPreservesColumnNames(t *testing.T) {
	// Regression test: column names with underscores should not be
	// auto-formatted (e.g. "user_name" must not become "USER NAME").
	buf := new(bytes.Buffer)
	columns := []string{"user_name", "created_at", "is_active"}
	rows := [][]string{{"alice", "2024-01-01", "true"}}

	table := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
		Formatting: tw.CellFormatting{AutoFormat: tw.Off},
	}))
	headers := make([]any, len(columns))
	for i, v := range columns {
		headers[i] = v
	}
	table.Header(headers...)
	for _, row := range rows {
		vals := make([]any, len(row))
		for i, v := range row {
			vals[i] = v
		}
		if err := table.Append(vals...); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.Render(); err != nil {
		t.Fatal(err)
	}

	result := buf.String()
	for _, col := range columns {
		if !strings.Contains(result, col) {
			t.Errorf("expected column name %q to be preserved in output, got:\n%s", col, result)
		}
	}
}

func Test_extractRangeText(t *testing.T) {
	type args struct {
		text      string
		startLine int
		startChar int
		endLine   int
		endChar   int
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "extract single line",
			args: args{
				text:      "select * from city",
				startLine: 0,
				startChar: 0,
				endLine:   0,
				endChar:   8,
			},
			want: "select *",
		},
		{
			name: "extract multi line with not equal start end",
			args: args{
				text:      "select 1;\nselect 2;\nselect 3;",
				startLine: 0,
				startChar: 7,
				endLine:   2,
				endChar:   8,
			},
			want: "1;\nselect 2;\nselect 3",
		},
		{
			name: "extract multi line with equal start end",
			args: args{
				text:      "select 1;\nselect 2;\nselect 3;",
				startLine: 1,
				startChar: 2,
				endLine:   1,
				endChar:   6,
			},
			want: "lect",
		},
		{
			name: "extract unicode using utf16 offsets",
			args: args{
				text:      "SELECT 'あ😀い';",
				startLine: 0,
				startChar: 8,
				endLine:   0,
				endChar:   12,
			},
			want: "あ😀い",
		},
		{
			name: "extract long single line without scanner limit",
			args: args{
				text:      strings.Repeat("a", 70000) + "SELECT",
				startLine: 0,
				startChar: 70000,
				endLine:   0,
				endChar:   70006,
			},
			want: "SELECT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractRangeText(tt.args.text, tt.args.startLine, tt.args.startChar, tt.args.endLine, tt.args.endChar); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
