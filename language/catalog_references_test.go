package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

var itemTable = catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ITEM"}

func catalogReferencesCache() *catalog.Cache {
	cache := readyNavigationCache()
	addNavigationTable(cache, "ITEM", "ID", "NAME")
	addNavigationTable(cache, "OTHER_ITEM", "ID")
	procedure := &catalog.ProcedureDesc{Name: "SET_ITEM"}
	cache.Catalog.Procedures["SET_ITEM"] = procedure
	cache.Catalog.ProceduresByID[(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "SET_ITEM"}).Key()] = procedure
	cache.Catalog.RebuildObjects(strings.ToUpper)
	return cache
}

// referenceTexts returns each reference's text, prefixed by its line, so a test
// can tell which of several same-named words was found.
func referenceTexts(t *testing.T, req language.Request, object catalog.ObjectID, column string) []string {
	t.Helper()
	result, err := language.NewService().CatalogReferences(context.Background(), req, object, column)
	if err != nil {
		t.Fatalf("CatalogReferences: %v", err)
	}
	lines := strings.Split(req.Document.Text, "\n")
	got := make([]string, 0, len(result.Value))
	for _, r := range result.Value {
		if r.Start.Line != r.End.Line {
			t.Fatalf("range %#v spans lines", r)
		}
		line := lines[r.Start.Line]
		got = append(got, strings.TrimSpace(line[:r.Start.UTF16Column])+"|"+line[r.Start.UTF16Column:r.End.UTF16Column])
	}
	return got
}

func expectReferences(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("references =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCatalogReferencesFindAColumnPastSameNamedLocals(t *testing.T) {
	text := "CREATE PROCEDURE P (ID INTEGER) RETURNS (N VARCHAR(10)) AS\nBEGIN\n" +
		"SELECT o.ID FROM OTHER_ITEM o WHERE o.ID = :ID INTO :N;\n" +
		"SELECT i.NAME FROM ITEM i\nWHERE i.ID = :ID INTO :N;\nEND"
	got := referenceTexts(t, navigationRequest(text, catalogReferencesCache()), itemTable, "ID")
	expectReferences(t, got, "WHERE i.|ID")
}

func TestCatalogReferencesFindAnUnqualifiedColumnBesideASameNamedLocal(t *testing.T) {
	text := "ALTER PROCEDURE P AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nSELECT ID FROM ITEM;\nEND"
	got := referenceTexts(t, navigationRequest(text, catalogReferencesCache()), itemTable, "ID")
	expectReferences(t, got, "SELECT|ID")
}

func TestCatalogReferencesFindTriggerRowColumns(t *testing.T) {
	text := "CREATE TRIGGER TR FOR ITEM ACTIVE BEFORE UPDATE POSITION 0 AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nID = 1;\nIF (NEW.ID <> OLD.ID) THEN EXIT;\nEND"
	got := referenceTexts(t, navigationRequest(text, catalogReferencesCache()), itemTable, "ID")
	expectReferences(t, got, "IF (NEW.|ID", "IF (NEW.ID <> OLD.|ID")

	other := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "OTHER_ITEM"}
	expectReferences(t, referenceTexts(t, navigationRequest(text, catalogReferencesCache()), other, "ID"))
}

func TestCatalogReferencesFindInsertColumns(t *testing.T) {
	text := "ALTER PROCEDURE P (ID INTEGER) AS\nBEGIN\nINSERT INTO ITEM (ID, NAME)\nVALUES (:ID, 'ID');\nEND"
	got := referenceTexts(t, navigationRequest(text, catalogReferencesCache()), itemTable, "ID")
	expectReferences(t, got, "INSERT INTO ITEM (|ID")
}

func TestCatalogReferencesFindARelationButNotItsSameNamedColumn(t *testing.T) {
	addItemColumn := func() *catalog.Cache {
		cache := catalogReferencesCache()
		addNavigationTable(cache, "HOLDER", "ITEM")
		return cache
	}
	text := "-- ITEM\nSELECT h.ITEM, 'ITEM'\nFROM HOLDER h JOIN ITEM i ON i.ID = h.ITEM"
	got := referenceTexts(t, navigationRequest(text, addItemColumn()), itemTable, "")
	expectReferences(t, got, "FROM HOLDER h JOIN|ITEM")
}

func TestCatalogReferencesFindAProcedureCall(t *testing.T) {
	text := "ALTER PROCEDURE P (SET_ITEM INTEGER) AS\nBEGIN\nEXECUTE PROCEDURE SET_ITEM :SET_ITEM;\nEND"
	procedure := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "SET_ITEM"}
	got := referenceTexts(t, navigationRequest(text, catalogReferencesCache()), procedure, "")
	expectReferences(t, got, "EXECUTE PROCEDURE|SET_ITEM")
}

func TestCatalogReferencesNeedCurrentInterBaseMetadata(t *testing.T) {
	text := "SELECT ID FROM ITEM"
	stale := navigationRequest(text, catalogReferencesCache())
	stale.Context.Snapshot.Stale = true
	expectReferences(t, referenceTexts(t, stale, itemTable, ""))

	other := navigationRequest(text, catalogReferencesCache())
	other.Context.Variant = dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL}
	expectReferences(t, referenceTexts(t, other, itemTable, ""))
}
