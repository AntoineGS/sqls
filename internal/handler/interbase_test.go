package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestInterBaseDialect1LanguageServerCompletion(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()
	configureInterBaseTestServer(t, tx)

	cases := []struct {
		name string
		text string
		col  int
		want string
		bad  string
	}{
		{
			name: "catalog identifier and column prefix",
			text: "select rdb$ from rdb$database",
			col:  11,
			want: "RDB$RELATION_ID",
			bad:  "RDB_OTHER",
		},
		{
			name: "lowercase user table",
			text: "select u. from users u",
			col:  9,
			want: "id",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.text)

			params := lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.col},
				},
			}
			var got []lsp.CompletionItem
			if err := tx.conn.Call(tx.ctx, "textDocument/completion", params, &got); err != nil {
				t.Fatal("conn.Call textDocument/completion:", err)
			}

			labels := handlerCompletionLabels(got)
			if !labels[tt.want] {
				t.Errorf("missing completion %q in %v", tt.want, labels)
			}
			if tt.bad != "" && labels[tt.bad] {
				t.Errorf("unexpected completion %q in %v", tt.bad, labels)
			}
		})
	}
}

func handlerCompletionLabels(items []lsp.CompletionItem) map[string]bool {
	labels := make(map[string]bool, len(items))
	for _, item := range items {
		labels[item.Label] = true
	}
	return labels
}

func TestInterBaseDialect1LanguageServerFormatting(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()
	configureInterBaseTestServer(t, tx)

	input := `select "literal" from rdb$database`
	tx.textDocumentDidOpen(t, testFileURI, input)
	params := lsp.DocumentFormattingParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
	}

	var got []lsp.TextEdit
	if err := tx.conn.Call(tx.ctx, "textDocument/formatting", params, &got); err != nil {
		t.Fatal("conn.Call textDocument/formatting:", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d text edits, want 1", len(got))
	}

	want := "SELECT\n\t\"literal\"\nFROM\n\trdb$database"
	if got[0].NewText != want {
		t.Fatalf("formatted InterBase Dialect 1 query = %q, want %q", got[0].NewText, want)
	}
}

func TestInterBaseDialect1StatementParsing(t *testing.T) {
	input := `select "a"";""b", 'c'';''d'; select rdb$database`
	statements, err := getStatementsWithDriver(input, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2", len(statements))
	}
	if got, want := statements[0].String(), `select "a"";""b", 'c'';''d';`; got != want {
		t.Errorf("first InterBase statement = %q, want %q", got, want)
	}
	if got, want := statements[1].String(), " select rdb$database"; got != want {
		t.Errorf("second InterBase statement = %q, want %q", got, want)
	}
}

func TestInterBaseDialect1LanguageServerHover(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()
	configureInterBaseTestServer(t, tx)

	input := "select rdb$relation_id from rdb$database"
	tx.textDocumentDidOpen(t, testFileURI, input)
	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 11},
		},
	}
	var got lsp.Hover
	if err := tx.conn.Call(tx.ctx, "textDocument/hover", params, &got); err != nil {
		t.Fatal("conn.Call textDocument/hover:", err)
	}
	if !strings.Contains(got.Contents.Value, "`RDB$RELATION_ID` column") || !strings.Contains(got.Contents.Value, "`INTEGER`") {
		t.Fatalf("hover = %q, want uppercase InterBase catalog metadata", got.Contents.Value)
	}
}

func TestParserDriverVariant(t *testing.T) {
	s := NewServer()

	if got, want := s.parserDriverVariant(), (dialect.DriverVariant{}); got != want {
		t.Errorf("parserDriverVariant() without a connection = %#v, want %#v", got, want)
	}
	if got := s.parserDriver(); got != "" {
		t.Errorf("parserDriver() without a connection = %q, want empty", got)
	}

	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	if got, want := s.parserDriverVariant().Driver, dialect.DatabaseDriverInterBase; got != want {
		t.Errorf("parserDriverVariant().Driver = %q, want %q", got, want)
	}
	if got, want := s.parserDriver(), dialect.DatabaseDriverInterBase; got != want {
		t.Errorf("parserDriver() = %q, want %q", got, want)
	}
}

func TestInterBaseStatementParsingByVariant(t *testing.T) {
	const input = `select "a"";""b", 'c'';''d'; select rdb$database`

	tests := []struct {
		name    string
		variant dialect.SQLVariant
	}{
		{name: "dialect 1", variant: dialect.SQLVariantInterBase1},
		{name: "dialect 3", variant: dialect.SQLVariantInterBase3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statements, err := getStatementsWithDriverVariant(input, dialect.DriverVariant{
				Driver:  dialect.DatabaseDriverInterBase,
				Variant: tt.variant,
			})
			if err != nil {
				t.Fatal(err)
			}
			// A semicolon inside a quoted run must not split the statement,
			// under either dialect.
			if len(statements) != 2 {
				t.Fatalf("got %d statements, want 2", len(statements))
			}
			if got, want := statements[0].String(), `select "a"";""b", 'c'';''d';`; got != want {
				t.Errorf("first statement = %q, want %q", got, want)
			}
			if got, want := statements[1].String(), " select rdb$database"; got != want {
				t.Errorf("second statement = %q, want %q", got, want)
			}
		})
	}
}

func configureInterBaseTestServer(t *testing.T, tx *TestContext) {
	t.Helper()

	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "RDB$DATABASE", Name: "RDB$RELATION_ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Table: "RDB$DATABASE", Name: "RDB_OTHER"}, Type: "VARCHAR(20)"},
		{ColumnBase: database.ColumnBase{Table: "users", Name: "id"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"RDB$DATABASE", "users"}}, nil
		},
		MockDescribeDatabaseTable: func(context.Context) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	if err := tx.server.worker.ReCache(context.Background(), repo); err != nil {
		t.Fatal("worker.ReCache:", err)
	}
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
}
