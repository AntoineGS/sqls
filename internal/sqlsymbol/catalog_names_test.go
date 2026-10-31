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

func TestCatalogNameSpansSkipCommentsLiteralsAndOtherNames(t *testing.T) {
	text := "/* ID */ SELECT i.ID, 'ID', \"ID\", \"Id\", id, IDX -- ID\nFROM ITEM i"
	var got []string
	for _, span := range analyzeInterBase(t, text).CatalogNameSpans("ID") {
		got = append(got, text[span.Start:span.End])
	}
	if want := []string{"ID", `"ID"`, "id"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spans = %q, want %q", got, want)
	}
}

func TestTriggerRowRelationNamesTheTriggersForRelation(t *testing.T) {
	text := "CREATE TRIGGER TR FOR ITEM ACTIVE BEFORE INSERT POSITION 0 AS\nDECLARE VARIABLE ID INTEGER;\nBEGIN\nID = NEW.ID + OLD.ID;\nEND"
	a := analyzeInterBase(t, text)
	for _, marker := range []string{"NEW.ID", "OLD.ID"} {
		relation, ok := a.TriggerRowRelation(strings.Index(text, marker) + len("NEW."))
		if !ok || relation.Key() != "ITEM" {
			t.Fatalf("%s: relation = %#v, %v; want ITEM", marker, relation, ok)
		}
	}
	for _, offset := range []int{strings.Index(text, "ID = "), strings.Index(text, "NEW.ID")} {
		if relation, ok := a.TriggerRowRelation(offset); ok {
			t.Fatalf("offset %d: relation = %#v, want none", offset, relation)
		}
	}
}

func TestTriggerRowRelationIgnoresNewOutsideATrigger(t *testing.T) {
	text := "SELECT NEW.ID FROM NEW"
	if relation, ok := analyzeInterBase(t, text).TriggerRowRelation(strings.Index(text, ".ID") + 1); ok {
		t.Fatalf("relation = %#v, want none", relation)
	}
}
