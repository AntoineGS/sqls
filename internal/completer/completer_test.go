package completer

import (
	"context"
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestGetBeforeCursorText(t *testing.T) {
	input := `SELECT
a, b, c
FROM
hogetable
`
	tests := []struct {
		in   string
		line int
		char int
		out  string
	}{
		{input, 1, 2, "SE"},
		{input, 2, 3, "SELECT\na, "},
		{input, 3, 4, "SELECT\na, b, c\nFROM"},
		{input, 4, 5, "SELECT\na, b, c\nFROM\nhoget"},
		{"select 'テスト', ci", 1, 16, "select 'テスト', ci"},
		{"select '😀', ci", 1, 15, "select '😀', ci"},
		{"select '😀', ci", 1, 13, "select '😀', "},
		{"select 1", 1, 100, "select 1"},
	}
	for _, tt := range tests {
		got := getBeforeCursorText(tt.in, tt.line, tt.char)
		if tt.out != got {
			t.Errorf("want %#v, got %#v", tt.out, got)
		}
	}
}

func TestGetLastWord(t *testing.T) {
	input := `SELECT
    a, b, c
FROM  
    hogetable
`
	tests := []struct {
		name string
		in   string
		line int
		char int
		out  string
	}{
		{"", "SELECT  FROM def", 1, 7, ""},
		{"", input, 1, 2, "SE"},
		{"", input, 2, 3, ""},
		{"", input, 3, 4, "FROM"},
		{"", input, 3, 6, ""},
		{"", input, 4, 5, "h"},
		{"", "`ident", 1, 6, "`ident"},
		{"", "parent.`ident", 1, 13, "`ident"},
		{"", "`parent`.`ident", 1, 15, "`ident"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLastWord(tt.in, tt.line, tt.char)
			if tt.out != got {
				t.Errorf("want %#v, got %#v", tt.out, got)
			}
		})
	}
}

func TestGetLastWordInterBaseDialect1(t *testing.T) {
	if got, want := getLastWordWithDriver("select rdb$", 1, 11, dialect.DatabaseDriverInterBase), "rdb$"; got != want {
		t.Fatalf("InterBase last word = %q, want %q", got, want)
	}
	if got := getLastWord("select rdb$", 1, 11); got != "" {
		t.Fatalf("generic last word = %q, want an empty match", got)
	}
}

func Test_completionTypeIs(t *testing.T) {
	type args struct {
	}
	tests := []struct {
		name            string
		completionTypes []completionType
		expect          completionType
		want            bool
	}{
		{
			completionTypes: []completionType{
				CompletionTypeColumn,
			},
			expect: CompletionTypeColumn,
			want:   true,
		},
		{
			completionTypes: []completionType{
				CompletionTypeTable,
				CompletionTypeView,
				CompletionTypeFunction,
				CompletionTypeColumn,
			},
			expect: CompletionTypeColumn,
			want:   true,
		},
		{
			completionTypes: []completionType{
				CompletionTypeTable,
				CompletionTypeView,
				CompletionTypeFunction,
			},
			expect: CompletionTypeColumn,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := completionTypeIs(tt.completionTypes, tt.expect); got != tt.want {
				t.Errorf("completionTypeIs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComplete(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		lowerCase bool
		expected  []lsp.CompletionItem
	}{
		{
			name: "keyword",
			text: "sel",
			expected: []lsp.CompletionItem{
				{
					Label:    "SELECT",
					Kind:     lsp.KeywordCompletion,
					Detail:   "keyword",
					SortText: "9999SELECT",
				},
			},
		},
		{
			name:      "keyword-lowercase",
			text:      "sel",
			lowerCase: true,
			expected: []lsp.CompletionItem{
				{
					Label:    "select",
					Kind:     lsp.KeywordCompletion,
					Detail:   "keyword",
					SortText: "9999select",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			c := NewCompleter(nil)
			got, err := c.Complete("sel", lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{
						Line:      0,
						Character: len(tt.text),
					},
				},
			}, tt.lowerCase)
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("\nwant: %v\ngot:  %v", tt.expected, got)
			}
		})
	}
}

func TestCompleteInterBaseDialect1DollarIdentifier(t *testing.T) {
	c := NewCompleter(interBaseCompletionCache(t))
	c.Driver = dialect.DatabaseDriverInterBase

	got, err := c.Complete("select rdb$ from rdb$database", lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: 11},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	labels := completionLabels(got)
	if !labels["RDB$RELATION_ID"] {
		t.Fatalf("missing InterBase catalog column in completions: %v", labels)
	}
	if labels["RDB_OTHER"] {
		t.Fatalf("completion prefix should include '$': %v", labels)
	}
}

func TestCompleteInterBaseDialect1KeepsLowercaseUserNames(t *testing.T) {
	c := NewCompleter(interBaseCompletionCache(t))
	c.Driver = dialect.DatabaseDriverInterBase

	got, err := c.Complete("select u. from users u", lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: 9},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	if !completionLabels(got)["id"] {
		t.Fatalf("missing lowercase user column in completions: %v", completionLabels(got))
	}
}

func TestCompleteInterBaseJoinMatchesUppercaseCatalogNames(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "lowercase tables with bare aliases",
			text: "select * from child c join parent p on ",
			want: "p.PARENT_A = c.CHILD_A AND p.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case tables with as aliases",
			text: "SELECT * FROM Child AS c JOIN Parent AS p ON ",
			want: "p.PARENT_A = c.CHILD_A AND p.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case tables without aliases",
			text: "SELECT * FROM Child JOIN Parent ON ",
			want: "Parent.PARENT_A = Child.CHILD_A AND Parent.PARENT_B = Child.CHILD_B",
		},
		{
			name: "mixed case tables with mixed case aliases",
			text: "select * from cHiLd childAlias join pArEnT parentAlias on ",
			want: "parentAlias.PARENT_A = childAlias.CHILD_A AND parentAlias.PARENT_B = childAlias.CHILD_B",
		},
		{
			name: "lowercase tables with bare aliases at join clause",
			text: "select * from child c join ",
			want: "PARENT P1 ON P1.PARENT_A = c.CHILD_A AND P1.PARENT_B = c.CHILD_B",
		},
		{
			name: "mixed case table with as alias at join clause",
			text: "SELECT * FROM Child AS c JOIN ",
			want: "PARENT P1 ON P1.PARENT_A = c.CHILD_A AND P1.PARENT_B = c.CHILD_B",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseJoinCompletionCache(t))
			c.Driver = dialect.DatabaseDriverInterBase

			got, err := c.Complete(tt.text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: len(tt.text)},
				},
			}, false)
			if err != nil {
				t.Fatal(err)
			}

			if !completionLabels(got)[tt.want] {
				t.Fatalf("missing composite foreign-key completion %q in %v", tt.want, completionLabels(got))
			}
		})
	}
}

func TestCompleteInterBaseJoinEscapesDollarInSnippet(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantLabel  string
		wantInsert string
	}{
		{
			name:       "join clause",
			text:       "select * from child$2 c$2 join ",
			wantLabel:  "PARENT$2 P1 ON P1.PARENT_ID$2 = c$2.CHILD_ID$2",
			wantInsert: "PARENT\\$2 ${1:P1} ON ${1:P1}.PARENT_ID\\$2 = c\\$2.CHILD_ID\\$2$0",
		},
		{
			name:       "join condition",
			text:       "select * from child$2 c$2 join parent$2 p$2 on ",
			wantLabel:  "p$2.PARENT_ID$2 = c$2.CHILD_ID$2",
			wantInsert: "p\\$2.PARENT_ID\\$2 = c\\$2.CHILD_ID\\$2$0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseDollarJoinCompletionCache(t))
			c.Driver = dialect.DatabaseDriverInterBase

			got, err := c.Complete(tt.text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: len(tt.text)},
				},
			}, false)
			if err != nil {
				t.Fatal(err)
			}

			var candidate *lsp.CompletionItem
			for i := range got {
				if got[i].Label == tt.wantLabel {
					candidate = &got[i]
					break
				}
			}
			if candidate == nil {
				t.Fatalf("missing completion label %q in %v", tt.wantLabel, completionLabels(got))
			}
			if candidate.InsertText != tt.wantInsert {
				t.Fatalf("completion %q InsertText = %q, want %q", tt.wantLabel, candidate.InsertText, tt.wantInsert)
			}
		})
	}
}

func interBaseCompletionCache(t *testing.T) *database.DBCache {
	t.Helper()

	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "RDB$DATABASE", Name: "RDB$RELATION_ID"}},
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "RDB$DATABASE", Name: "RDB_OTHER"}},
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "users", Name: "id"}},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "MAIN", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{"MAIN"}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"MAIN": {"RDB$DATABASE", "users"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}

	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func interBaseJoinCompletionCache(t *testing.T) *database.DBCache {
	t.Helper()

	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "CHILD", Name: "CHILD_A"}},
		{ColumnBase: database.ColumnBase{Table: "CHILD", Name: "CHILD_B"}},
		{ColumnBase: database.ColumnBase{Table: "PARENT", Name: "PARENT_A"}},
		{ColumnBase: database.ColumnBase{Table: "PARENT", Name: "PARENT_B"}},
	}
	foreignKeys := []*database.ForeignKey{
		{
			[2]*database.ColumnBase{
				{Table: "CHILD", Name: "CHILD_A"},
				{Table: "PARENT", Name: "PARENT_A"},
			},
			[2]*database.ColumnBase{
				{Table: "CHILD", Name: "CHILD_B"},
				{Table: "PARENT", Name: "PARENT_B"},
			},
		},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"CHILD", "PARENT"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return foreignKeys, nil
		},
	}

	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func interBaseDollarJoinCompletionCache(t *testing.T) *database.DBCache {
	t.Helper()

	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "CHILD$2", Name: "CHILD_ID$2"}},
		{ColumnBase: database.ColumnBase{Table: "PARENT$2", Name: "PARENT_ID$2"}},
	}
	foreignKeys := []*database.ForeignKey{
		{
			[2]*database.ColumnBase{
				{Table: "CHILD$2", Name: "CHILD_ID$2"},
				{Table: "PARENT$2", Name: "PARENT_ID$2"},
			},
		},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"CHILD$2", "PARENT$2"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return foreignKeys, nil
		},
	}

	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func completionLabels(items []lsp.CompletionItem) map[string]bool {
	labels := make(map[string]bool, len(items))
	for _, item := range items {
		labels[item.Label] = true
	}
	return labels
}

func TestGenerateAlias(t *testing.T) {
	noMatchesTable := make(map[string]interface{})
	noMatchesTable["XX"] = true
	matchesTable := make(map[string]interface{})
	matchesTable["XX"] = true
	matchesTable["T1"] = true

	tests := []struct {
		name  string
		table string
		tMap  map[string]interface{}
		want  string
	}{
		{
			"no matches",
			"Table",
			noMatchesTable,
			"T1",
		},
		{
			"matches",
			"Table",
			matchesTable,
			"T2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateTableAlias(tt.table, tt.tMap); got != tt.want {
				t.Errorf("generateAlias() = %v, want  %v", got, tt.want)
			}
		})
	}
}
