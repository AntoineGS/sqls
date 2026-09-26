package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

type countingDetailReader struct{ calls int }

func (r *countingDetailReader) Detail(context.Context, metadata.Request) (metadata.Detail, error) {
	r.calls++
	return metadata.Detail{}, nil
}

func TestMergedDiagnosticPolicyControlsNullComparison(t *testing.T) {
	base := language.Request{
		Document: language.Document{URI: "memory:policy", Text: "SELECT ID FROM T WHERE ID = NULL;"},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	service := language.NewService()
	find := func(req language.Request) (int, bool) {
		result, err := service.Diagnostics(context.Background(), req)
		if err != nil {
			t.Fatalf("Diagnostics() error = %v", err)
		}
		for _, diagnostic := range result.Value {
			if diagnostic.Code == "interbase-null-comparison" {
				return diagnostic.Severity, true
			}
		}
		return 0, false
	}
	if severity, ok := find(base); !ok || severity != 2 {
		t.Fatalf("default null-comparison = (%d, %v), want warning severity 2", severity, ok)
	}
	base.Policy = language.DiagnosticPolicy{Rules: map[string]string{"interbase-null-comparison": "off"}}
	if severity, ok := find(base); ok {
		t.Fatalf("off null-comparison emitted severity %d", severity)
	}
	base.Policy = language.DiagnosticPolicy{Rules: map[string]string{"interbase-null-comparison": "error"}}
	if severity, ok := find(base); !ok || severity != 1 {
		t.Fatalf("error null-comparison = (%d, %v), want error severity 1", severity, ok)
	}
}

func TestValidateDiagnosticPolicyRejectsUnknownCodeAndLevel(t *testing.T) {
	for name, policy := range map[string]language.DiagnosticPolicy{
		"code":  {Rules: map[string]string{"not-a-rule": "error"}},
		"level": {Rules: map[string]string{"interbase-null-comparison": "critical"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := language.ValidateDiagnosticPolicy(policy); err == nil {
				t.Fatal("ValidateDiagnosticPolicy() error = nil, want invalid policy error")
			}
		})
	}
}

func TestDiagnosticsRejectsInvalidPolicyAndMapsNonBMPSpans(t *testing.T) {
	service := language.NewService()
	request := language.Request{
		Document: language.Document{URI: "memory:unicode", Text: "SELECT '😀' FROM T WHERE ID = NULL;"},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
		Policy: language.DiagnosticPolicy{Rules: map[string]string{"interbase-null-comparison": "invalid"}},
	}
	if _, err := service.Diagnostics(context.Background(), request); err == nil {
		t.Fatal("Diagnostics() error = nil for invalid rule level")
	}
	request.Policy = language.DiagnosticPolicy{}
	result, err := service.Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	for _, diagnostic := range result.Value {
		if diagnostic.Code != "interbase-null-comparison" {
			continue
		}
		if diagnostic.Range.Start.Line != 0 || diagnostic.Range.Start.UTF16Column != 25 {
			t.Fatalf("diagnostic start=%+v, want line 0 UTF-16 column 25", diagnostic.Range.Start)
		}
		return
	}
	t.Fatal("missing null-comparison finding")
}

func TestNullComparisonDiagnosticsDoNotWaitForColumns(t *testing.T) {
	service := language.NewService()
	request := language.Request{
		Document: language.Document{URI: "memory:loading", Text: "SELECT ID FROM T WHERE ID = NULL;"},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	result, err := service.Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	for _, diagnostic := range result.Value {
		if diagnostic.Code == "interbase-null-comparison" {
			return
		}
	}
	t.Fatal("text-only null-comparison diagnostic missing while catalog columns are loading")
}

func TestPublicDiagnosticsKeepsOptionalRuleDefaultOff(t *testing.T) {
	details := &countingDetailReader{}
	request := language.Request{
		Document: language.Document{URI: "memory:default-off", Text: "CREATE PROCEDURE X (A INTEGER) AS DECLARE VARIABLE B SMALLINT; BEGIN B = A; END;"},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
			Details:         details,
		},
	}
	result, err := language.NewService().Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	for _, diagnostic := range result.Value {
		if diagnostic.Code == "interbase-lossy-assignment" {
			t.Fatalf("default-off lossy assignment emitted %+v", diagnostic)
		}
	}
	if details.calls != 0 {
		t.Fatalf("diagnostic request performed %d provider detail calls", details.calls)
	}
}

func TestPublicDiagnosticsReportsTooManyProcedureArgumentsWhenMinimumIsUnknown(t *testing.T) {
	procedureKey := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "P"}.Key()
	snapshot := &catalog.MetadataSnapshot{Cache: &catalog.Cache{
		Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{
			procedureKey: {Name: "P", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "A", Type: "INTEGER"}, {Name: "B", Type: "INTEGER"}}},
		}},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataProcedures: catalog.MetadataReady},
	}}
	request := language.Request{
		Document: language.Document{URI: "memory:arity", Text: "EXECUTE PROCEDURE P(1, 2, 3);"},
		Context: language.Context{
			Snapshot: snapshot, Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, DialectResolved: true,
		},
	}
	result, err := language.NewService().Diagnostics(context.Background(), request)
	if err != nil {
		t.Fatalf("Diagnostics() error = %v", err)
	}
	for _, diagnostic := range result.Value {
		if diagnostic.Code == "interbase-procedure-arity" {
			return
		}
	}
	t.Fatalf("too-many-arguments finding missing; diagnostics=%+v", result.Value)
}
