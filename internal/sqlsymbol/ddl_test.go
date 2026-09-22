package sqlsymbol

import (
	"strings"
	"testing"
)

func TestColumnDeclarationSkipsCommentsAndConstraints(t *testing.T) {
	ddl := `CREATE TABLE "CUSTOMERINVOICE" (
  /* AMOUNTPAID is documented here */
  "AMOUNTPAID_OLD" INTEGER,
  "AMOUNTPAID" NUMERIC(15,2),
  CONSTRAINT "AMOUNTPAID_CHECK" CHECK ("AMOUNTPAID" >= 0)
);`
	span, ok := ColumnDeclaration(ddl,
		Name{Text: "CUSTOMERINVOICE", Quoted: true},
		Name{Text: "AMOUNTPAID", Quoted: true})
	if !ok {
		t.Fatal("column declaration not found")
	}
	want := strings.Index(ddl, `"AMOUNTPAID" NUMERIC`)
	if span.Start != want || ddl[span.Start:span.End] != `"AMOUNTPAID"` {
		t.Fatalf("wrong column range: %+v", span)
	}
}

func TestTableDeclarationScansGeneratedHeaders(t *testing.T) {
	cases := []struct {
		name  string
		ddl   string
		table Name
	}{
		{"global temporary", `CREATE GLOBAL TEMPORARY TABLE "CUSTOMERINVOICE" ("ID" INTEGER)`, Name{Text: "CUSTOMERINVOICE", Quoted: true}},
		{"external file punctuation", `CREATE TABLE "CUSTOMERINVOICE" EXTERNAL FILE '../customer-invoice/data.v1' ("ID" INTEGER)`, Name{Text: "CUSTOMERINVOICE", Quoted: true}},
		{"dialect 1 generated quoted names", `CREATE TABLE "CUSTOMERINVOICE" ("ID" INTEGER)`, Name{Text: "CUSTOMERINVOICE", Quoted: true}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			span, ok := TableDeclaration(tt.ddl, tt.table)
			if !ok || tt.ddl[span.Start:span.End] != `"CUSTOMERINVOICE"` {
				t.Fatalf("TableDeclaration() = %+v, %v", span, ok)
			}
		})
	}
}

func TestColumnDeclarationViewExplicitListAndRejectsLineage(t *testing.T) {
	ddl := `CREATE VIEW "INVOICE_VIEW" ("BALANCE", "INVOICE") AS SELECT AMOUNT, ID FROM X`
	span, ok := ColumnDeclaration(ddl, Name{Text: "INVOICE_VIEW", Quoted: true}, Name{Text: "BALANCE", Quoted: true})
	if !ok || ddl[span.Start:span.End] != `"BALANCE"` {
		t.Fatalf("explicit view column declaration = %+v, %v", span, ok)
	}
	if _, ok := ColumnDeclaration(ddl, Name{Text: "INVOICE_VIEW", Quoted: true}, Name{Text: "AMOUNT", Quoted: true}); ok {
		t.Fatal("inferred SELECT lineage was treated as a declaration")
	}
}
