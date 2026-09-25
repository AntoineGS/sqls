package language_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func signatureProcedureCache() *catalog.Cache {
	return &catalog.Cache{
		Catalog: &catalog.CatalogCache{
			Procedures: map[string]*catalog.ProcedureDesc{
				"MYPROC": {
					Name: "MYPROC",
					InputParameters: []*catalog.ProcedureParameterDesc{
						{Name: "IN_CODE", Position: 0, Direction: catalog.ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
						{Name: "IN_AMOUNT", Position: 1, Direction: catalog.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
					},
					OutputParameters: []*catalog.ProcedureParameterDesc{
						{Name: "OUT_TOTAL", Position: 0, Direction: catalog.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
					},
				},
			},
		},
	}
}

func wantProcedureSignatureHelp(activeParameter int) language.SignatureHelp {
	return language.SignatureHelp{
		Signatures: []language.Signature{
			{
				Label:         "MYPROC (IN_CODE, IN_AMOUNT)",
				Documentation: "MYPROC procedure — 2 input parameters, 1 output parameter",
				Parameters: []language.Parameter{
					{Label: "IN_CODE", Documentation: "`VARCHAR(3)` input NOT NULL"},
					{Label: "IN_AMOUNT", Documentation: "`NUMERIC(18, 2)` input"},
				},
			},
		},
		ActiveSignature: 0,
		ActiveParameter: activeParameter,
	}
}

func signatureRequest(cache *catalog.Cache, text string) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:signature", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Cache: cache},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
}

func TestSignatureHelpForExecuteProcedure(t *testing.T) {
	const input = "execute procedure myproc(123, 45)"
	cases := []struct {
		col                 int
		wantActiveParameter int
	}{
		{col: 25, wantActiveParameter: 0},
		{col: 28, wantActiveParameter: 0},
		{col: 29, wantActiveParameter: 1},
		{col: 31, wantActiveParameter: 1},
	}
	req := signatureRequest(signatureProcedureCache(), input)
	for _, tt := range cases {
		result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: tt.col})
		if err != nil {
			t.Fatalf("col %d: %v", tt.col, err)
		}
		if diff := cmp.Diff(wantProcedureSignatureHelp(tt.wantActiveParameter), result.Value); diff != "" {
			t.Errorf("col %d: signature help mismatch (-want +got):\n%s", tt.col, diff)
		}
	}
}

func TestSignatureHelpForSelectableProcedureCall(t *testing.T) {
	// Reached without EXECUTE PROCEDURE at all: the trigger is "the callee
	// is a known procedure", not a specific keyword.
	const input = "select * from myproc("
	req := signatureRequest(signatureProcedureCache(), input)
	result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: len(input)})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantProcedureSignatureHelp(0), result.Value); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestSignatureHelpUnknownCalleeReturnsEmpty(t *testing.T) {
	for _, input := range []string{
		"select upper(",
		"execute procedure nosuchproc(",
		"select * from city",
	} {
		req := signatureRequest(signatureProcedureCache(), input)
		result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: len(input)})
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if len(result.Value.Signatures) != 0 {
			t.Errorf("SignatureHelp(%q) = %+v, want no signatures", input, result.Value)
		}
	}
}

func TestSignatureHelpWithoutCatalogReturnsEmpty(t *testing.T) {
	const input = "execute procedure myproc("
	req := signatureRequest(&catalog.Cache{}, input)
	result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: len(input)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value.Signatures) != 0 {
		t.Errorf("SignatureHelp = %+v, want no signatures with no catalog", result.Value)
	}
}

func TestSignatureHelpForInsertValues(t *testing.T) {
	cache := &catalog.Cache{
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"): {
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER", Key: "YES"},
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "NAME"}, Type: "VARCHAR(35)"},
			},
		},
	}
	const input = "insert into city (ID, Name) VALUES (123, "
	req := signatureRequest(cache, input)
	result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: len(input)})
	if err != nil {
		t.Fatal(err)
	}
	want := language.SignatureHelp{
		Signatures: []language.Signature{
			{
				Label:         "city (ID, Name)",
				Documentation: "city table columns",
				Parameters: []language.Parameter{
					{Label: "ID", Documentation: "`INTEGER` PRIMARY KEY"},
					{Label: "Name", Documentation: "`VARCHAR(35)`"},
				},
			},
		},
		ActiveSignature: 0,
		ActiveParameter: 1,
	}
	if diff := cmp.Diff(want, result.Value); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestSignatureHelpNilSnapshotReturnsEmpty(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:signature", Text: "execute procedure myproc(", Version: 1, Binding: 1},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	result, err := language.NewService().SignatureHelp(context.Background(), req, language.Position{Line: 0, UTF16Column: 26})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value.Signatures) != 0 {
		t.Errorf("SignatureHelp = %+v, want no signatures for a nil-catalog snapshot", result.Value)
	}
}
