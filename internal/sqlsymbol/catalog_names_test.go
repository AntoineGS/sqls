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
	triggers := analyzeInterBase(t, text).Triggers(nil)
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

// alterTriggerRowColumns returns, per recognized trigger, its relation key
// and the text before each row column, so a test can tell NEW from OLD.
func alterTriggerRowColumns(t *testing.T, text string, relations TriggerCatalog) []string {
	t.Helper()
	var got []string
	for _, trg := range analyzeInterBase(t, text).Triggers(relations) {
		entry := trg.Relation.Key() + ":"
		for _, span := range trg.RowColumns {
			entry += " " + text[span.Start-4:span.End]
		}
		got = append(got, entry)
	}
	return got
}

func TestTriggersReportAlterTriggerRowColumnsOfTheCatalogRelation(t *testing.T) {
	text := "ALTER TRIGGER TR ACTIVE BEFORE UPDATE POSITION 1 AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nID = NEW.ID + OLD.ID;\nEND"
	triggers := analyzeInterBase(t, text).Triggers(newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}}))
	if len(triggers) != 1 {
		t.Fatalf("triggers = %#v, want one", triggers)
	}
	trg := triggers[0]
	if trg.Relation.Key() != "ITEM" || trg.Span.Start != 0 || trg.Span.End != len(text) {
		t.Fatalf("trigger = %#v", trg)
	}
	if got := alterTriggerRowColumns(t, text, newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}})); strings.Join(got, ",") != "ITEM: NEW.ID OLD.ID" {
		t.Fatalf("row columns = %q", got)
	}
	if !trg.Locals["ID"] || len(trg.Locals) != 1 {
		t.Fatalf("locals = %v, want ID", trg.Locals)
	}
}

// Without a proven relation an ALTER TRIGGER keeps its span and locals but
// reports no row columns.
func TestTriggersWithholdAlterTriggerRowColumnsWithoutACatalogRelation(t *testing.T) {
	body := " AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nID = NEW.ID;\nEND"
	for name, tc := range map[string]struct {
		text      string
		relations TriggerCatalog
	}{
		"no catalog":        {"ALTER TRIGGER TR" + body, nil},
		"unknown trigger":   {"ALTER TRIGGER TR" + body, newTriggerFixtureCatalog(map[string]Name{"OTHER": {Text: "ITEM"}})},
		"empty relation":    {"ALTER TRIGGER TR" + body, newTriggerFixtureCatalog(map[string]Name{"TR": {}})},
		"dropped earlier":   {"DROP TRIGGER TR;\nALTER TRIGGER TR" + body, newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}})},
		"altered earlier":   {"ALTER TRIGGER TR INACTIVE;\nALTER TRIGGER TR" + body, newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}})},
		"quoted not folded": {`ALTER TRIGGER "tr"` + body, newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}})},
	} {
		t.Run(name, func(t *testing.T) {
			triggers := analyzeInterBase(t, tc.text).Triggers(tc.relations)
			if len(triggers) == 0 {
				t.Fatalf("ALTER TRIGGER not recognized")
			}
			trg := triggers[len(triggers)-1]
			if trg.Relation.Key() != "" || len(trg.RowColumns) != 0 || !trg.Locals["ID"] {
				t.Fatalf("trigger = %#v, want no relation or row columns, local ID", trg)
			}
		})
	}
}

// An earlier CREATE of the same trigger supplies its own FOR relation, not
// the catalog's, to that CREATE; the later ALTER is withheld.
func TestTriggersWithholdAlterTriggerCreatedEarlierInTheDocument(t *testing.T) {
	text := "CREATE TRIGGER TR FOR OTHER BEFORE INSERT AS\nBEGIN\nNEW.ID = 1;\nEND;\nALTER TRIGGER TR AS\nBEGIN\nNEW.ID = 2;\nEND"
	got := alterTriggerRowColumns(t, text, newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}}))
	if strings.Join(got, ",") != "OTHER: NEW.ID,:" {
		t.Fatalf("triggers = %q", got)
	}
}

// An ALTER TRIGGER's catalog relation is stale once the document earlier
// creates, alters or drops that relation, so its row columns are withheld.
func TestTriggersWithholdAlterTriggerRowColumnsOfARelationChangedEarlier(t *testing.T) {
	body := "ALTER TRIGGER TR AS BEGIN NEW.ID = 1; END"
	catalog := newTriggerFixtureCatalog(map[string]Name{"TR": {Text: "ITEM"}})
	for _, prefix := range []string{"DROP TABLE ITEM;\n", "ALTER TABLE ITEM ADD X INTEGER;\n", "CREATE TABLE ITEM (ID INTEGER);\n"} {
		trg := analyzeInterBase(t, prefix+body).Triggers(catalog)
		if len(trg) != 1 || len(trg[0].RowColumns) != 0 {
			t.Fatalf("%q: triggers = %#v, want no row columns", prefix, trg)
		}
	}
	if got := alterTriggerRowColumns(t, "DROP TABLE OTHER;\n"+body, catalog); strings.Join(got, ",") != "ITEM: NEW.ID" {
		t.Fatalf("unrelated DDL: triggers = %q", got)
	}
}

// A CREATE TRIGGER's relation may be redefined earlier in the document, so its
// row columns are withheld exactly as diagnostics skip them.
func TestTriggersWithholdCreateTriggerRowColumnsOfARelationChangedEarlier(t *testing.T) {
	body := "CREATE TRIGGER TR FOR ITEM BEFORE INSERT AS BEGIN NEW.ID = 1; END"
	for _, prefix := range []string{"DROP TABLE ITEM;\n", "ALTER TABLE ITEM ADD X INTEGER;\n", "CREATE TABLE ITEM (ID INTEGER);\n"} {
		trg := analyzeInterBase(t, prefix+body).Triggers(nil)
		if len(trg) != 1 || len(trg[0].RowColumns) != 0 {
			t.Fatalf("%q: triggers = %#v, want no row columns", prefix, trg)
		}
	}
	if got := alterTriggerRowColumns(t, "DROP TABLE OTHER;\n"+body, nil); strings.Join(got, ",") != "ITEM: NEW.ID" {
		t.Fatalf("unrelated DDL: triggers = %q", got)
	}
}

func TestTriggersMatchAQuotedAlterTriggerNameExactly(t *testing.T) {
	text := `ALTER TRIGGER "tr" AS BEGIN NEW.ID = 1; END`
	got := alterTriggerRowColumns(t, text, newTriggerFixtureCatalog(map[string]Name{"tr": {Text: "ITEM"}}))
	if strings.Join(got, ",") != "ITEM: NEW.ID" {
		t.Fatalf("triggers = %q", got)
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
