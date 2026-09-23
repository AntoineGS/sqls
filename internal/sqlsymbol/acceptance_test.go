package sqlsymbol

import (
	"os"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestLocalInterBaseExample(t *testing.T) {
	path := os.Getenv("SQLS_SYMBOL_EXAMPLE")
	if path == "" {
		t.Skip("SQLS_SYMBOL_EXAMPLE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	a, err := Analyze(text, dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(text, "\n")
	for _, target := range []struct {
		line int
		name string
	}{
		{line: 265, name: "import_order_line_items"},
		{line: 266, name: "import_order_payment"},
	} {
		if target.line > len(lines) {
			t.Fatalf("example has %d lines; want line %d", len(lines), target.line)
		}
		line := lines[target.line-1]
		column := strings.Index(line, target.name)
		if column < 0 {
			t.Fatalf("expected %s on example line %d", target.name, target.line)
		}
		lineOffset := column
		for index := 0; index < target.line-1; index++ {
			lineOffset += len(lines[index]) + 1
		}
		resolution := a.Resolve(lineOffset)
		if resolution.Role != Relation || resolution.SQL == nil || !resolution.SQL.Name.MatchesCatalogName(strings.ToUpper(target.name)) {
			t.Fatalf("line %d relation resolution = %+v, want exact table %s", target.line, resolution, strings.ToUpper(target.name))
		}
	}
	needle := "IMPORTEXTERNALORDER_EMPLYID(:HEADEREMPLYID_TEMP"
	call := strings.Index(text, needle)
	if call < 0 {
		t.Fatal("expected example call not found")
	}
	offset := call + len("IMPORTEXTERNALORDER_EMPLYID(:")
	r := a.Resolve(offset)
	if r.Role != Local || r.Symbol == nil {
		t.Fatalf("resolution: %+v", r)
	}
	if n := len(a.References(r.Symbol, true)); n != 3 {
		t.Fatalf("HEADEREMPLYID_TEMP has %d occurrences, want 3", n)
	}
	if _, err := a.Rename(r.Symbol, "HEADER_EMPLOYEE_TEMP"); err != nil {
		t.Fatal(err)
	}

	t.Run("CUSTOMERINVOICE update references", func(t *testing.T) {
		upper := strings.ToUpper(text)
		update := strings.Index(upper, "UPDATE CUSTOMERINVOICE")
		if update < 0 {
			t.Skip("CUSTOMERINVOICE UPDATE is not present in this example")
		}
		orderTotal := strings.Index(upper[update:], "ORDERTOTAL")
		if orderTotal < 0 {
			t.Fatal("expected ORDERTOTAL in UPDATE not found")
		}
		order := a.Resolve(update + orderTotal)
		if order.Role != Local || order.Symbol == nil || order.Symbol.Name.Key() != "ORDERTOTAL" {
			t.Fatalf("ORDERTOTAL resolution: %+v", order)
		}
		declaration := order.Symbol.Declaration
		if declaration.Start < 0 || declaration.End > len(text) || declaration.Start >= declaration.End {
			t.Fatalf("ORDERTOTAL declaration has invalid span: %+v", declaration)
		}
		declarationText := text[declaration.Start:declaration.End]
		declarationLine := strings.Count(text[:declaration.Start], "\n") + 1
		if declarationText != "ORDERTOTAL" || declarationLine != 55 {
			t.Fatalf("ORDERTOTAL resolved from UPDATE to declaration %q on line %d (span %+v), want exact ORDERTOTAL token on line 55", declarationText, declarationLine, declaration)
		}

		amountPaid := strings.Index(upper[update:], "AMOUNTPAID")
		if amountPaid < 0 {
			t.Fatal("expected AMOUNTPAID target not found")
		}
		column := a.Resolve(update + amountPaid)
		if column.Role != Column || column.SQL == nil {
			t.Fatalf("AMOUNTPAID resolution: %+v, want column with SQL candidates", column)
		}
		owned := false
		for _, scope := range column.SQL.Scopes {
			for _, relation := range scope {
				if relation.Name.Key() == "CUSTOMERINVOICE" {
					owned = true
				}
			}
		}
		if !owned {
			t.Fatalf("AMOUNTPAID candidates do not include CUSTOMERINVOICE: %+v", column.SQL.Scopes)
		}
	})
}
