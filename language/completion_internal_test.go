package language

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

// This file ports internal/completer's unit tests over its unexported
// helpers (getBeforeCursorText, getLastWord*, completionTypeIs,
// getSortTextPrefix, generateTableAlias) now that internal/completer has
// been deleted as a dead fork; language.Service.Complete is production's
// only completion path, so these helpers' coverage belongs here.

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

func TestGetLastWordWithVariantMatchesDriverForm(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	if got, want := getLastWordWithVariant("select rdb$", 1, 11, dv), "rdb$"; got != want {
		t.Errorf("getLastWordWithVariant() = %q, want %q", got, want)
	}
	if got, want := getLastWordWithVariant("select rdb$", 1, 11, dialect.DriverVariant{}), ""; got != want {
		t.Errorf("getLastWordWithVariant() with no driver = %q, want %q", got, want)
	}
	if got, want := getLastWordWithDriver("select rdb$", 1, 11, dialect.DatabaseDriverInterBase), "rdb$"; got != want {
		t.Errorf("getLastWordWithDriver() = %q, want %q", got, want)
	}
}

func TestCompletionTypeIs(t *testing.T) {
	tests := []struct {
		name            string
		completionTypes []completionType
		expect          completionType
		want            bool
	}{
		{
			completionTypes: []completionType{completionTypeColumn},
			expect:          completionTypeColumn,
			want:            true,
		},
		{
			completionTypes: []completionType{completionTypeTable, completionTypeView, completionTypeFunction, completionTypeColumn},
			expect:          completionTypeColumn,
			want:            true,
		},
		{
			completionTypes: []completionType{completionTypeTable, completionTypeView, completionTypeFunction},
			expect:          completionTypeColumn,
			want:            false,
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

func TestCompletionKindSortPrefixesAreAssigned(t *testing.T) {
	cases := []struct {
		kind CompletionKind
		want string
	}{
		{KindField, "0"},
		{KindClass, "1"},
		{KindFunction, "10"},
		{KindMethod, "10"},
		{KindValue, "11"},
		{KindKeyword, "9999"},
	}
	for _, tt := range cases {
		if got := getSortTextPrefix(tt.kind); got != tt.want {
			t.Errorf("getSortTextPrefix(%v) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	// The failure this pins: a new kind left in the catch-all list sorts below
	// every keyword, which is worse than not offering it.
	for _, kind := range []CompletionKind{KindMethod, KindValue} {
		if getSortTextPrefix(kind) == getSortTextPrefix(KindKeyword) {
			t.Errorf("kind %v is still in the keyword sort bucket", kind)
		}
	}
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
		{"no matches", "Table", noMatchesTable, "T1"},
		{"matches", "Table", matchesTable, "T2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := generateTableAlias(tt.table, tt.tMap); got != tt.want {
				t.Errorf("generateAlias() = %v, want  %v", got, tt.want)
			}
		})
	}
}
