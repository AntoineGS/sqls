package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func analyzeInterBase(t *testing.T, text string) *Analysis {
	t.Helper()
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCatalogNameSpansSkipCommentsLiteralsVariablesAndOtherNames(t *testing.T) {
	text := "/* ID */ SELECT i.ID, 'ID', \"ID\", \"Id\", id, IDX, :ID -- ID\nFROM ITEM i"
	var got []string
	for _, occurrence := range analyzeInterBase(t, text).CatalogNameSpans("ID") {
		got = append(got, text[occurrence.Span.Start:occurrence.Span.End])
	}
	if want := []string{"ID", `"ID"`, "id"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spans = %q, want %q", got, want)
	}
}

func TestTriggersReportForRelationRowColumnsAndLocals(t *testing.T) {
	text := "SELECT NEW.ID FROM NEW;\nCREATE TRIGGER TR FOR ITEM ACTIVE BEFORE INSERT POSITION 0 AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nID = NEW.ID + OLD.ID;\nEND"
	triggers := analyzeInterBase(t, text).Triggers()
	if len(triggers) != 1 {
		t.Fatalf("triggers = %#v, want one", triggers)
	}
	trg := triggers[0]
	if trg.Relation.Key() != "ITEM" || text[trg.Span.Start:trg.Span.End] != text[strings.Index(text, "CREATE"):] {
		t.Fatalf("trigger = %#v", trg)
	}
	var rows []int
	for _, span := range trg.RowColumns {
		rows = append(rows, span.Start)
	}
	if want := []int{strings.Index(text, "NEW.ID +") + 4, strings.Index(text, "OLD.ID") + 4}; len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("row columns = %v, want %v", rows, want)
	}
	if !trg.Locals["ID"] || len(trg.Locals) != 1 {
		t.Fatalf("locals = %v, want ID", trg.Locals)
	}
}

func TestDefinitionColumnProverAnswersSeveralOffsets(t *testing.T) {
	text := "ALTER PROCEDURE P AS\nDECLARE VARIABLE DBID INTEGER;\nBEGIN\nSELECT DBID FROM DATABASEID;\nSELECT DBID FROM DATABASEID;\nEND"
	catalog := newDiagnosticFixtureCatalog()
	catalog.relations["DATABASEID"] = RelationFact{Columns: []ColumnFact{{Name: "DBID", Type: "INTEGER"}}, ColumnsKnown: true}
	prove := analyzeInterBase(t, text).DefinitionColumnProver(catalog)
	for _, offset := range []int{strings.Index(text, "SELECT DBID") + 7, strings.LastIndex(text, "SELECT DBID") + 7} {
		if got, ok := prove(offset); !ok || got.Relation.Key() != "DATABASEID" || got.Column.Key() != "DBID" {
			t.Fatalf("offset %d: proof = %#v, %v", offset, got, ok)
		}
	}
	if _, ok := prove(strings.Index(text, "VARIABLE DBID") + 9); ok {
		t.Fatal("the declaration is not a column")
	}
}
