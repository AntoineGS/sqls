package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func navRequest(text string, driver dialect.DatabaseDriver) language.Request {
	return language.Request{Document: language.Document{URI: "file:///nav.sql", Text: text}, Context: language.Context{Variant: dialect.DriverVariant{Driver: driver}}}
}

func TestNavigationDefinitionPrefersLocalAlias(t *testing.T) {
	text := "SELECT c.id FROM city AS c"
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: 0, UTF16Column: 7})
	if err != nil {
		t.Fatal(err)
	}
	want := language.Range{Start: language.Position{Line: 0, UTF16Column: 25}, End: language.Position{Line: 0, UTF16Column: 26}}
	if len(got.Value) != 1 || got.Value[0].URI != req.Document.URI || got.Value[0].Range != want || got.Value[0].Source != nil {
		t.Fatalf("alias definition = %#v, want local alias span %+v", got.Value, want)
	}
}

func TestNavigationProcedureLocalReferencesAndRename(t *testing.T) {
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv = 0;\nv = :v + 1; -- v\nSELECT 'v' FROM t;\nEND"
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	service := language.NewService()
	for _, tc := range []struct {
		include bool
		want    int
	}{{false, 3}, {true, 4}} {
		refs, err := service.References(context.Background(), req, language.Position{Line: 4, UTF16Column: 5}, tc.include)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs.Value) != tc.want {
			t.Fatalf("includeDeclaration=%v refs=%d want %d: %#v", tc.include, len(refs.Value), tc.want, refs.Value)
		}
	}
	renamed, err := service.Rename(context.Background(), req, language.Position{Line: 4, UTF16Column: 5}, "amount")
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.Value) != 1 || len(renamed.Value[0].Edits) != 4 {
		t.Fatalf("rename = %#v, want declaration and three use edits (comments/strings excluded)", renamed.Value)
	}
}

func TestNavigationProcedureLocalDefinitionAndShadowing(t *testing.T) {
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv=0;\nEND"
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: 3, UTF16Column: 0})
	if err != nil {
		t.Fatal(err)
	}
	want := language.Range{Start: language.Position{Line: 1, UTF16Column: 17}, End: language.Position{Line: 1, UTF16Column: 18}}
	if len(got.Value) != 1 || got.Value[0].Range != want || got.Value[0].URI != req.Document.URI {
		t.Fatalf("local definition = %#v, want declaration %+v", got.Value, want)
	}

	shadow := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv=1;\nEND"
	req = navRequest(shadow, dialect.DatabaseDriverInterBase)
	refs, err := language.NewService().References(context.Background(), req, language.Position{Line: 4, UTF16Column: 0}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Value) != 0 {
		t.Fatalf("ambiguous shadowed procedure locals references = %#v, want none", refs.Value)
	}
}

func TestNavigationLocalSymbolRangesUseUTF16WithEmojiAndCRLF(t *testing.T) {
	text := "ALTER PROCEDURE p AS\r\nDECLARE VARIABLE v INTEGER;\r\nBEGIN\r\nv='😀'; v=1;\r\nEND"
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: 3, UTF16Column: 0})
	if err != nil {
		t.Fatal(err)
	}
	want := language.Range{Start: language.Position{Line: 1, UTF16Column: 17}, End: language.Position{Line: 1, UTF16Column: 18}}
	if len(got.Value) != 1 || got.Value[0].Range != want {
		t.Fatalf("UTF-16/CRLF definition range = %#v, want %+v", got.Value, want)
	}
	refs, err := language.NewService().References(context.Background(), req, language.Position{Line: 3, UTF16Column: 8}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Value) != 3 || refs.Value[2].Range != (language.Range{Start: language.Position{Line: 3, UTF16Column: 8}, End: language.Position{Line: 3, UTF16Column: 9}}) {
		t.Fatalf("UTF-16 reference spans = %#v, want second same-line use at columns 8-9", refs.Value)
	}
}

func TestNavigationNonInterBaseAliasDefinitionFallback(t *testing.T) {
	req := navRequest("SELECT c.id FROM city AS c", dialect.DatabaseDriverPostgreSQL)
	got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: 0, UTF16Column: 7})
	if err != nil {
		t.Fatal(err)
	}
	want := language.Range{Start: language.Position{Line: 0, UTF16Column: 25}, End: language.Position{Line: 0, UTF16Column: 26}}
	if len(got.Value) != 1 || got.Value[0].Range != want {
		t.Fatalf("non-InterBase alias definition = %#v, want %+v", got.Value, want)
	}
}
