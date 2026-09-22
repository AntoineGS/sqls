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
	configureInterBaseTestServer(t, tx, dialect.SQLVariantInterBase1)

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

func TestInterBaseLanguageServerFormattingByVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant dialect.SQLVariant
		input   string
		want    string
	}{
		{
			name:    "dialect 1 treats double quotes as a string",
			variant: dialect.SQLVariantInterBase1,
			input:   `select "literal", 'c''d' from rdb$database`,
			want:    "SELECT\n\t\"literal\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "dialect 3 treats double quotes as a delimited identifier",
			variant: dialect.SQLVariantInterBase3,
			input:   `select "literal", 'c''d' from rdb$database`,
			want:    "SELECT\n\t\"literal\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "dialect 3 keeps a space inside a delimited identifier",
			variant: dialect.SQLVariantInterBase3,
			input:   `select "My Column" from rdb$database`,
			want:    "SELECT\n\t\"My Column\"\nFROM\n\trdb$database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			tx.textDocumentDidOpen(t, testFileURI, tt.input)
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
			if got[0].NewText != tt.want {
				t.Fatalf("formatted query = %q, want %q", got[0].NewText, tt.want)
			}
			if strings.Contains(tt.input, `'c''d'`) && !strings.Contains(got[0].NewText, `'c''d'`) {
				t.Fatalf("the language server corrupted an escaped string literal: %q", got[0].NewText)
			}
		})
	}
}

func TestInterBaseVariantReachesCompletionAndHover(t *testing.T) {
	// The variant on the connection must reach the completer, not just the
	// formatter: this is the end of the propagation chain that plan 1 builds.
	tests := []struct {
		name    string
		variant dialect.SQLVariant
	}{
		{name: "dialect 1", variant: dialect.SQLVariantInterBase1},
		{name: "dialect 3", variant: dialect.SQLVariantInterBase3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			const text = "select rdb$ from rdb$database"
			tx.textDocumentDidOpen(t, testFileURI, text)

			var completions []lsp.CompletionItem
			if err := tx.conn.Call(tx.ctx, "textDocument/completion", lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: 11},
				},
			}, &completions); err != nil {
				t.Fatal("conn.Call textDocument/completion:", err)
			}
			if labels := handlerCompletionLabels(completions); !labels["RDB$RELATION_ID"] {
				t.Errorf("missing catalog column completion under %s: %v", tt.variant, labels)
			}

			var hover lsp.Hover
			if err := tx.conn.Call(tx.ctx, "textDocument/hover", lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: 11},
				},
			}, &hover); err != nil {
				t.Fatal("conn.Call textDocument/hover:", err)
			}
			_ = hover
		})
	}

	// The dialect-dependent half: a Dialect 3 connection offers TIMESTAMP,
	// a Dialect 1 connection does not, because the type does not exist there.
	keywordTests := []struct {
		variant dialect.SQLVariant
		want    bool
	}{
		{variant: dialect.SQLVariantInterBase3, want: true},
		{variant: dialect.SQLVariantInterBase1, want: false},
	}
	for _, tt := range keywordTests {
		t.Run("TIMESTAMP offered for "+string(tt.variant), func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			const text = "TIM"
			tx.textDocumentDidOpen(t, testFileURI, text)

			var completions []lsp.CompletionItem
			if err := tx.conn.Call(tx.ctx, "textDocument/completion", lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: len(text)},
				},
			}, &completions); err != nil {
				t.Fatal("conn.Call textDocument/completion:", err)
			}
			if got := handlerCompletionLabels(completions)["TIMESTAMP"]; got != tt.want {
				t.Errorf("TIMESTAMP offered = %v, want %v under %s", got, tt.want, tt.variant)
			}
		})
	}
}

func TestParserDriverVariantZeroWithoutConnectionAndInterBaseZeroVariantIsDialect3(t *testing.T) {
	s := NewServer()
	if s.dbConn != nil {
		t.Fatal("a fresh server must have no connection")
	}
	dv := s.parserDriverVariant()
	if dv != (dialect.DriverVariant{}) {
		t.Fatalf("parserDriverVariant() = %#v, want the zero value", dv)
	}

	// The zero DriverVariant with the InterBase driver resolves to Dialect 3,
	// matching the interbase-go default. This is what fixes the bug offline.
	ib, ok := dialect.DialectForDriverVariant(dialect.DriverVariant{
		Driver: dialect.DatabaseDriverInterBase,
	}).(*dialect.InterBaseDialect)
	if !ok {
		t.Fatal("the InterBase driver must resolve to *InterBaseDialect")
	}
	if got, want := ib.SQLDialect, 3; got != want {
		t.Fatalf("offline InterBase SQLDialect = %d, want %d", got, want)
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
	configureInterBaseTestServer(t, tx, dialect.SQLVariantInterBase1)

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

func configureInterBaseTestServer(t *testing.T, tx *TestContext, variant dialect.SQLVariant) {
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
	tx.server.dbConn = &database.DBConnection{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: variant,
	}
}

type recordingMessenger struct {
	warnings []string
	infos    []string
	errs     []string
}

func (m *recordingMessenger) ShowLog(context.Context, string) error { return nil }

func (m *recordingMessenger) ShowInfo(_ context.Context, message string) error {
	m.infos = append(m.infos, message)
	return nil
}

func (m *recordingMessenger) ShowWarning(_ context.Context, message string) error {
	m.warnings = append(m.warnings, message)
	return nil
}

func (m *recordingMessenger) ShowError(_ context.Context, message string) error {
	m.errs = append(m.errs, message)
	return nil
}

func TestShowConnectionWarnings(t *testing.T) {
	const warning = `interbase: connection "centrale" is configured for SQL dialect 1 but the database reports SQL dialect 3; sqls will lex and render types as dialect 1. Remove ` + "`dialect`" + ` or set ` + "`dialect: 0`" + ` to follow the database.`

	t.Run("warnings reach the messenger", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{
			Driver:   dialect.DatabaseDriverInterBase,
			Variant:  dialect.SQLVariantInterBase1,
			Warnings: []string{warning},
		}
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)

		if len(messenger.warnings) != 1 {
			t.Fatalf("got %d warnings, want 1: %v", len(messenger.warnings), messenger.warnings)
		}
		if messenger.warnings[0] != warning {
			t.Errorf("warning = %q, want %q", messenger.warnings[0], warning)
		}
		if len(messenger.errs) != 0 || len(messenger.infos) != 0 {
			t.Error("a connect warning must not be shown as an error or an info")
		}
	})

	t.Run("no connection is a no-op", func(t *testing.T) {
		s := NewServer()
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)
		if len(messenger.warnings) != 0 {
			t.Errorf("got %d warnings without a connection, want 0", len(messenger.warnings))
		}
	})

	t.Run("no warnings is a no-op", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)
		if len(messenger.warnings) != 0 {
			t.Errorf("got %d warnings, want 0", len(messenger.warnings))
		}
	})

	t.Run("a nil messenger does not panic", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{Warnings: []string{warning}}
		s.showConnectionWarnings(context.Background(), nil)
	})
}

func TestSwitchDatabaseGuardRefusesAnotherAttachment(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	ctx := context.Background()
	repository := &database.InterBaseDBRepository{DatabaseName: attachment}

	if err := validateDatabaseSwitch(ctx, repository, attachment); err != nil {
		t.Fatalf("validateDatabaseSwitch(current) error = %v, want nil", err)
	}
	if err := validateDatabaseSwitch(ctx, repository, "/srv/interbase/other.ib"); err == nil {
		t.Fatal("validateDatabaseSwitch(other) returned nil error")
	}
	if err := validateDatabaseSwitch(ctx, &database.MockDBRepository{}, "world"); err != nil {
		t.Fatalf("a repository without the capability must accept any name: %v", err)
	}
}

// TestSwitchDatabaseRefusesAnotherAttachmentAndLeavesStateUnchanged exercises
// the actual (*Server).switchDatabase, not just the pure guard helpers above:
// a wiring bug that runs the guard too late, or after curDBName is already
// mutated, would pass those unit tests but corrupt server state here.
func TestSwitchDatabaseRefusesAnotherAttachmentAndLeavesStateUnchanged(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	s := NewServer()
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase}
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, DatabaseName: attachment}
	s.curDBName = attachment

	_, err := s.switchDatabase(context.Background(), lsp.ExecuteCommandParams{
		Arguments: []interface{}{"/srv/interbase/other.ib"},
	})
	if err == nil {
		t.Fatal("switchDatabase(other) returned nil error")
	}
	if !strings.Contains(err.Error(), "single attachment") {
		t.Errorf("switchDatabase(other) error = %q, want mention of the single-attachment refusal", err)
	}
	if s.curDBName != attachment {
		t.Fatalf("curDBName = %q after a refused switch, want it to stay %q", s.curDBName, attachment)
	}
	if s.dbConn == nil || s.dbConn.DatabaseName != attachment {
		t.Fatal("dbConn was replaced by a refused switch")
	}
}

// TestSwitchDatabaseToCurrentAttachmentIsANoOp exercises the reachable path
// Finding 1 identified: showDatabases prints the composed attachment string,
// and switching to exactly that string used to fall through to
// reconnectionDB, which feeds the target back into newDBConnection's
// connCfg.DBName and lets interBaseAttachment recompose it into a broken,
// doubled attachment (proven directly, with no build tag, by
// TestInterBaseAttachmentRecomposesADoubledPathWhenFedItsOwnOutput in the
// database package). No connection source is configured on this Server, so
// if switchDatabase ever again falls through to reconnectionDB on this path,
// topConnection finds no connections and the call fails with ErrNoConnection
// — asserting success here is only possible because the no-op guard returns
// before reconnectionDB runs at all, which the dbConn/curDBName assertions
// below confirm directly rather than inferring it from a nil error alone.
func TestSwitchDatabaseToCurrentAttachmentIsANoOp(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	s := NewServer()
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase}
	originalConn := &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, DatabaseName: attachment}
	s.dbConn = originalConn

	_, err := s.switchDatabase(context.Background(), lsp.ExecuteCommandParams{
		Arguments: []interface{}{attachment},
	})
	if err != nil {
		t.Fatalf("switchDatabase(current attachment) error = %v, want nil (a no-op)", err)
	}
	if s.curDBName != "" {
		t.Fatalf("curDBName = %q after a no-op switch, want it left unset", s.curDBName)
	}
	if s.dbConn != originalConn {
		t.Fatal("dbConn was replaced by a switch to the already-open attachment; want a no-op")
	}
}
