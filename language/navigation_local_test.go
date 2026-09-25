package language_test

import (
	"context"
	"strings"
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
		wantRanges := []language.Range{
			{Start: language.Position{Line: 3, UTF16Column: 0}, End: language.Position{Line: 3, UTF16Column: 1}},
			{Start: language.Position{Line: 4, UTF16Column: 0}, End: language.Position{Line: 4, UTF16Column: 1}},
			{Start: language.Position{Line: 4, UTF16Column: 5}, End: language.Position{Line: 4, UTF16Column: 6}},
		}
		if tc.include {
			wantRanges = append([]language.Range{{Start: language.Position{Line: 1, UTF16Column: 17}, End: language.Position{Line: 1, UTF16Column: 18}}}, wantRanges...)
		}
		if len(refs.Value) != len(wantRanges) {
			t.Fatalf("reference ranges count = %d, want %d", len(refs.Value), len(wantRanges))
		}
		for i, want := range wantRanges {
			if refs.Value[i].Range != want {
				t.Fatalf("reference %d range = %+v, want %+v", i, refs.Value[i].Range, want)
			}
		}
	}
	colonRefs, err := service.References(context.Background(), req, language.Position{Line: 4, UTF16Column: 4}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(colonRefs.Value) != 3 || colonRefs.Value[2].Range.Start.UTF16Column != 5 {
		t.Fatalf("colon-cursor references = %#v, want the identifier-only local spans", colonRefs.Value)
	}
	renamed, err := service.Rename(context.Background(), req, language.Position{Line: 4, UTF16Column: 5}, "amount")
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.Value) != 1 || len(renamed.Value[0].Edits) != 4 {
		t.Fatalf("rename = %#v, want declaration and three use edits (comments/strings excluded)", renamed.Value)
	}
	wantEdits := []language.Range{
		{Start: language.Position{Line: 1, UTF16Column: 17}, End: language.Position{Line: 1, UTF16Column: 18}},
		{Start: language.Position{Line: 3, UTF16Column: 0}, End: language.Position{Line: 3, UTF16Column: 1}},
		{Start: language.Position{Line: 4, UTF16Column: 0}, End: language.Position{Line: 4, UTF16Column: 1}},
		{Start: language.Position{Line: 4, UTF16Column: 5}, End: language.Position{Line: 4, UTF16Column: 6}},
	}
	for i, want := range wantEdits {
		edit := renamed.Value[0].Edits[i]
		if edit.Range != want || edit.Text != "amount" {
			t.Fatalf("rename edit %d = %+v, want span %+v replacing with amount", i, edit, want)
		}
	}
	colonRename, err := service.Rename(context.Background(), req, language.Position{Line: 4, UTF16Column: 4}, "amount")
	if err != nil {
		t.Fatal(err)
	}
	if len(colonRename.Value) != 1 || len(colonRename.Value[0].Edits) != 4 || colonRename.Value[0].Edits[3].Range.Start.UTF16Column != 5 {
		t.Fatalf("colon-cursor rename = %#v, want identifier-only edits", colonRename.Value)
	}
	if text[strings.Index(text, "v = :v")+4] != ':' {
		t.Fatal("source colon changed during rename analysis")
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

func TestNavigationReferencesDoNotReportRelationCursorAsReference(t *testing.T) {
	for _, text := range []string{
		"SELECT * FROM customers",
		"SELECT c.id FROM customers c",
		"SELECT value FROM customers WHERE value = 1",
	} {
		t.Run(text, func(t *testing.T) {
			req := navRequest(text, dialect.DatabaseDriverPostgreSQL)
			got, err := language.NewService().References(context.Background(), req, language.Position{Line: 0, UTF16Column: 17}, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.Value == nil || len(got.Value) != 0 {
				t.Fatalf("relation references = %#v, want non-nil empty", got.Value)
			}
		})
	}
}

func TestNavigationLegacyRenamePreservesSpellingMatchesAndUTF16Ranges(t *testing.T) {
	text := "-- 😀\nSELECT '😀'; SELECT city.ID, city.Name FROM city WHERE city.ID = 'city' -- city\nSELECT city.ID FROM city"
	req := navRequest(text, dialect.DatabaseDriverPostgreSQL)
	got, err := language.NewService().Rename(context.Background(), req, language.Position{Line: 1, UTF16Column: 23}, "town")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Value) != 1 || len(got.Value[0].Edits) != 6 {
		t.Fatalf("legacy spelling rename = %#v, want six parsed identifier edits", got.Value)
	}
	wantStarts := []language.Position{{Line: 1, UTF16Column: 20}, {Line: 1, UTF16Column: 29}, {Line: 1, UTF16Column: 44}, {Line: 1, UTF16Column: 55}, {Line: 2, UTF16Column: 7}, {Line: 2, UTF16Column: 20}}
	for i, edit := range got.Value[0].Edits {
		if edit.Range.Start != wantStarts[i] || edit.Text != "town" {
			t.Errorf("edit %d = %+v, want start %+v replacing with town", i, edit, wantStarts[i])
		}
	}
	if got.Value[0].URI != req.Document.URI || got.Value[0].Shape != language.RenameDocumentChanges {
		t.Fatalf("legacy rename discriminator = %#v, want documentChanges for %s", got.Value[0], req.Document.URI)
	}
}

func TestNavigationRenameReportsInterBaseProcedureRoleErrors(t *testing.T) {
	service := language.NewService()
	for _, tc := range []struct {
		name string
		text string
		pos  language.Position
		want string
	}{
		{name: "ambiguous", text: "ALTER PROCEDURE p AS\nDECLARE VARIABLE duplicate INTEGER;\nDECLARE VARIABLE duplicate INTEGER;\nBEGIN\nduplicate=1;\nEND", pos: language.Position{Line: 4}, want: "cannot rename procedure target: ambiguous symbol"},
		{name: "sql relation", text: "ALTER PROCEDURE p AS BEGIN SELECT * FROM customer; END", pos: language.Position{Line: 0, UTF16Column: 42}, want: "cannot rename SQL target: database column or relation"},
		{name: "sql column", text: "ALTER PROCEDURE p AS BEGIN UPDATE customer SET amount = 1; END", pos: language.Position{Line: 0, UTF16Column: 48}, want: "cannot rename SQL target: database column or relation"},
		{name: "callable", text: "ALTER PROCEDURE p AS BEGIN EXECUTE PROCEDURE otherproc; END", pos: language.Position{Line: 0, UTF16Column: 46}, want: "cannot rename procedure target: other procedure or callable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.Rename(context.Background(), navRequest(tc.text, dialect.DatabaseDriverInterBase), tc.pos, "replacement")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Rename error = %v, want %q", err, tc.want)
			}
		})
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

func TestNavigationDefinitionDoesNotUseUnrelatedAliasForSQLRoles(t *testing.T) {
	text := "SELECT customer FROM other AS customer;\nSELECT id FROM customer;"
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	for _, tc := range []struct {
		line int
		text string
	}{
		{line: 0, text: "customer FROM"}, // column spelling matches an alias elsewhere
		{line: 1, text: "FROM customer"}, // relation spelling matches an alias elsewhere
	} {
		line := strings.Split(text, "\n")[tc.line]
		column := strings.Index(line, tc.text)
		if tc.line == 0 {
			column += strings.Index(line[column:], "customer")
		}
		if tc.line == 1 {
			column += strings.Index(line[column:], "customer")
		}
		got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: tc.line, UTF16Column: column})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Value) != 0 {
			t.Fatalf("SQL role at line %d resolved unrelated alias: %#v", tc.line, got.Value)
		}
	}
}

func TestNavigationDefinitionFindsAliasInsideProcedureSQL(t *testing.T) {
	text := "ALTER PROCEDURE p AS BEGIN SELECT c.id FROM city AS c; END"
	cursor := strings.Index(text, "c.id")
	alias := strings.LastIndex(text, " AS c") + len(" AS ")
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, language.Position{Line: 0, UTF16Column: cursor})
	if err != nil {
		t.Fatal(err)
	}
	want := language.Range{Start: language.Position{Line: 0, UTF16Column: alias}, End: language.Position{Line: 0, UTF16Column: alias + 1}}
	if len(got.Value) != 1 || got.Value[0].URI != req.Document.URI || got.Value[0].Range != want {
		t.Fatalf("procedure alias definition = %#v, want scoped alias at %+v", got.Value, want)
	}
}

func TestNavigationDefinitionCorrelatedAliasUsesOuterDeclaration(t *testing.T) {
	text := "SELECT c.id FROM customer c WHERE EXISTS (SELECT o.id FROM orders o WHERE c.id = o.id);"
	declaration := strings.Index(text, "customer c") + len("customer ")
	use := strings.LastIndex(text, "WHERE c.id") + len("WHERE ")
	assertAliasDefinitionAt(t, text, use, declaration)
}

func TestNavigationDefinitionReusedAliasUsesBindingIdentity(t *testing.T) {
	text := "SELECT c.id FROM customer c WHERE c.id IN (SELECT c.id FROM customer c WHERE c.id = c.id);"
	outerDeclaration := strings.Index(text, "customer c") + len("customer ")
	innerDeclaration := strings.LastIndex(text, "customer c") + len("customer ")
	outerUse := strings.Index(text, "WHERE c.id") + len("WHERE ")
	innerWhere := strings.Index(text[outerUse+1:], "WHERE c.id") + outerUse + 1
	innerUse := innerWhere + len("WHERE ")
	assertAliasDefinitionAt(t, text, outerUse, outerDeclaration)
	assertAliasDefinitionAt(t, text, innerUse, innerDeclaration)
}

func TestNavigationDefinitionAmbiguousSameScopeAliasReturnsNoLocation(t *testing.T) {
	text := "SELECT c.id FROM customer c JOIN orders c ON c.id = c.id"
	use := strings.Index(text, "SELECT ") + len("SELECT ")
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, testPosition(text, use))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Value) != 0 {
		t.Fatalf("ambiguous same-scope alias resolved to %#v, want none", got.Value)
	}
}

func assertAliasDefinitionAt(t *testing.T, text string, use, declaration int) {
	t.Helper()
	req := navRequest(text, dialect.DatabaseDriverInterBase)
	got, err := language.NewService().Definition(context.Background(), req, testPosition(text, use))
	if err != nil {
		t.Fatal(err)
	}
	wantStart := testPosition(text, declaration)
	want := language.Range{Start: wantStart, End: language.Position{Line: wantStart.Line, UTF16Column: wantStart.UTF16Column + 1}}
	if len(got.Value) != 1 || got.Value[0].URI != req.Document.URI || got.Value[0].Range != want {
		t.Fatalf("alias at byte %d resolved to %#v, want declaration byte %d range %+v", use, got.Value, declaration, want)
	}
}

func testPosition(text string, offset int) language.Position {
	position, err := language.PositionAt(text, offset)
	if err != nil {
		panic(err)
	}
	return position
}
