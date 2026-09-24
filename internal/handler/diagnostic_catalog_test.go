package handler

import (
	"database/sql"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func diagnosticName(text string) sqlsymbol.Name {
	return sqlsymbol.Name{Text: text}
}

func diagnosticQuotedName(text string) sqlsymbol.Name {
	return sqlsymbol.Name{Text: text, Quoted: true}
}

func TestDiagnosticCatalogRelationLoadingReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataLoading,
			database.MetadataViews:     database.MetadataLoading,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticName("MISSING_NAME")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown while relations are loading", knowledge)
	}
}

func TestDiagnosticCatalogRelationsReadyViewsIncompleteReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataLoading,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticName("MISSING_NAME")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown while the view namespace is incomplete", knowledge)
	}
	// A relevant, already-enumerated relation must still resolve Present even
	// though the namespace as a whole cannot yet prove absence.
	if _, knowledge := catalog.RelationInfo(diagnosticName("T")); knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present for an already-known relation", knowledge)
	}
}

func TestDiagnosticCatalogCompleteRelationNamespaceReportsMissing(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		Catalog:      &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticName("MISSING_NAME")); knowledge != sqlsymbol.Missing {
		t.Fatalf("knowledge = %v, want Missing for a complete namespace", knowledge)
	}
}

func TestDiagnosticCatalogPresentRelationWithLoadingColumns(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		Catalog:      &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	fact, knowledge := catalog.RelationInfo(diagnosticName("T"))
	if knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present", knowledge)
	}
	if fact.ColumnsKnown {
		t.Fatalf("fact.ColumnsKnown = true, want false while columns are loading")
	}
	if fact.Columns != nil {
		t.Fatalf("fact.Columns = %+v, want nil while columns are loading", fact.Columns)
	}
}

func TestDiagnosticCatalogPresentRelationWithReadyColumns(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{
			"\tT": {
				{ColumnBase: database.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"},
				{ColumnBase: database.ColumnBase{Table: "T", Name: "V"}, Type: "INTEGER", Null: "YES"},
			},
		},
		Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations:      database.MetadataReady,
			database.MetadataViews:          database.MetadataReady,
			database.MetadataColumnsCurrent: database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	fact, knowledge := catalog.RelationInfo(diagnosticName("T"))
	if knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present", knowledge)
	}
	if !fact.ColumnsKnown {
		t.Fatal("fact.ColumnsKnown = false, want true once columns are ready")
	}
	want := []sqlsymbol.ColumnFact{
		{Name: "ID", Type: "INTEGER", Nullability: sqlsymbol.NotNullable},
		{Name: "V", Type: "INTEGER", Nullability: sqlsymbol.Nullable},
	}
	if len(fact.Columns) != len(want) {
		t.Fatalf("fact.Columns = %+v, want %+v", fact.Columns, want)
	}
	for i := range want {
		if fact.Columns[i] != want[i] {
			t.Fatalf("fact.Columns[%d] = %+v, want %+v", i, fact.Columns[i], want[i])
		}
	}
}

func TestDiagnosticCatalogSystemRelationAlwaysUnknown(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		Catalog:      &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticName("RDB$RELATIONS")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown for an unsupported system relation despite a complete user namespace", knowledge)
	}
}

func TestDiagnosticCatalogFailedViewsReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataFailed,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticName("MISSING_NAME")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown when views failed to load", knowledge)
	}
}

func TestDiagnosticCatalogFailedProceduresReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataProcedures: database.MetadataFailed,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.ProcedureInfo(diagnosticName("MISSING_PROC")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown when procedures failed to load", knowledge)
	}
}

func TestDiagnosticCatalogFailedDomainsReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{Domains: map[string]*database.DomainDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataDomains: database.MetadataFailed,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.DomainInfo(diagnosticName("MISSING_DOMAIN")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown when domains failed to load", knowledge)
	}
}

func TestDiagnosticCatalogExactQuotedNames(t *testing.T) {
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"MixedCase"}},
		Catalog:      &database.CatalogCache{Views: map[string]*database.ViewDesc{}},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations: database.MetadataReady,
			database.MetadataViews:     database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.RelationInfo(diagnosticQuotedName("MixedCase")); knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present for the exact quoted spelling", knowledge)
	}
	if _, knowledge := catalog.RelationInfo(diagnosticQuotedName("MIXEDCASE")); knowledge != sqlsymbol.Missing {
		t.Fatalf("knowledge = %v, want Missing for a quoted name that does not match the exact catalog spelling", knowledge)
	}
	if _, knowledge := catalog.RelationInfo(diagnosticName("MixedCase")); knowledge != sqlsymbol.Missing {
		t.Fatalf("knowledge = %v, want Missing for an unquoted name folded to a spelling absent from the catalog", knowledge)
	}
}

func TestDiagnosticCatalogCompleteProcedureWithZeroParameters(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"P": {Name: "P"},
			},
		},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataProcedures: database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	fact, knowledge := catalog.ProcedureInfo(diagnosticName("P"))
	if knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present", knowledge)
	}
	if !fact.InputsKnown || len(fact.Inputs) != 0 {
		t.Fatalf("fact.Inputs = %+v (known=%v), want an empty, known list", fact.Inputs, fact.InputsKnown)
	}
	if !fact.OutputsKnown || len(fact.Outputs) != 0 {
		t.Fatalf("fact.Outputs = %+v (known=%v), want an empty, known list", fact.Outputs, fact.OutputsKnown)
	}
	if !fact.MinInputsKnown || fact.MinInputs != 0 {
		t.Fatalf("fact.MinInputs = %d (known=%v), want 0, known", fact.MinInputs, fact.MinInputsKnown)
	}
}

func TestDiagnosticCatalogProcedureWithParametersHasUnknownMinInputs(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"P": {
					Name: "P",
					InputParameters: []*database.ProcedureParameterDesc{
						{Name: "A", Position: 1, Direction: database.ParameterInput, Type: "INTEGER", Nullable: sql.NullBool{Bool: false, Valid: true}},
						{Name: "B", Position: 2, Direction: database.ParameterInput, Type: "INTEGER", Nullable: sql.NullBool{Bool: true, Valid: true}},
					},
					OutputParameters: []*database.ProcedureParameterDesc{
						{Name: "C", Position: 1, Direction: database.ParameterOutput, Type: "INTEGER"},
					},
				},
			},
		},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataProcedures: database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	fact, knowledge := catalog.ProcedureInfo(diagnosticName("P"))
	if knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present", knowledge)
	}
	wantInputs := []sqlsymbol.ColumnFact{
		{Name: "A", Type: "INTEGER", Nullability: sqlsymbol.NotNullable},
		{Name: "B", Type: "INTEGER", Nullability: sqlsymbol.Nullable},
	}
	if !fact.InputsKnown || len(fact.Inputs) != len(wantInputs) {
		t.Fatalf("fact.Inputs = %+v (known=%v), want %+v", fact.Inputs, fact.InputsKnown, wantInputs)
	}
	for i := range wantInputs {
		if fact.Inputs[i] != wantInputs[i] {
			t.Fatalf("fact.Inputs[%d] = %+v, want %+v", i, fact.Inputs[i], wantInputs[i])
		}
	}
	wantOutputs := []sqlsymbol.ColumnFact{{Name: "C", Type: "INTEGER", Nullability: sqlsymbol.NullUnknown}}
	if !fact.OutputsKnown || len(fact.Outputs) != len(wantOutputs) || fact.Outputs[0] != wantOutputs[0] {
		t.Fatalf("fact.Outputs = %+v (known=%v), want %+v", fact.Outputs, fact.OutputsKnown, wantOutputs)
	}
	if fact.MinInputsKnown {
		t.Fatalf("fact.MinInputsKnown = true, want false: this catalog cannot see trailing default parameters")
	}
}

func TestDiagnosticCatalogUnsupportedProceduresReportsUnknown(t *testing.T) {
	cache := &database.DBCache{
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataProcedures: database.MetadataUnsupported,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	if _, knowledge := catalog.ProcedureInfo(diagnosticName("MISSING_PROC")); knowledge != sqlsymbol.Unknown {
		t.Fatalf("knowledge = %v, want Unknown when the repository does not support procedures", knowledge)
	}
}

func TestDiagnosticCatalogDomainKnowledge(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{
			Domains: map[string]*database.DomainDesc{
				"D_AMOUNT": {Name: "D_AMOUNT", Type: "NUMERIC(15,2)", Nullable: sql.NullBool{Bool: false, Valid: true}},
			},
		},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataDomains: database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)
	fact, knowledge := catalog.DomainInfo(diagnosticName("D_AMOUNT"))
	if knowledge != sqlsymbol.Present {
		t.Fatalf("knowledge = %v, want Present", knowledge)
	}
	if fact.Type != "NUMERIC(15,2)" || fact.Nullability != sqlsymbol.NotNullable {
		t.Fatalf("fact = %+v, want type NUMERIC(15,2) and NotNullable", fact)
	}
	if _, knowledge := catalog.DomainInfo(diagnosticName("MISSING_DOMAIN")); knowledge != sqlsymbol.Missing {
		t.Fatalf("knowledge = %v, want Missing for a complete domain namespace", knowledge)
	}
}

func TestDiagnosticCatalogSnapshotIsImmuneToSourceMutation(t *testing.T) {
	procedure := &database.ProcedureDesc{
		Name: "P",
		InputParameters: []*database.ProcedureParameterDesc{
			{Name: "A", Position: 1, Direction: database.ParameterInput, Type: "INTEGER"},
		},
	}
	view := &database.ViewDesc{
		Name: "V",
		Columns: []*database.ColumnDesc{
			{ColumnBase: database.ColumnBase{Table: "V", Name: "ID"}, Type: "INTEGER"},
		},
	}
	domain := &database.DomainDesc{Name: "D_AMOUNT", Type: "NUMERIC(15,2)"}
	cache := &database.DBCache{
		SchemaTables: map[string][]string{"": {"T"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{
			"\tT": {{ColumnBase: database.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER"}},
		},
		Catalog: &database.CatalogCache{
			Views:      map[string]*database.ViewDesc{"V": view},
			Procedures: map[string]*database.ProcedureDesc{"P": procedure},
			Domains:    map[string]*database.DomainDesc{"D_AMOUNT": domain},
		},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataRelations:      database.MetadataReady,
			database.MetadataViews:          database.MetadataReady,
			database.MetadataColumnsCurrent: database.MetadataReady,
			database.MetadataProcedures:     database.MetadataReady,
			database.MetadataDomains:        database.MetadataReady,
		},
	}
	catalog := snapshotDiagnosticCatalog(cache)

	beforeTable, _ := catalog.RelationInfo(diagnosticName("T"))
	beforeView, _ := catalog.RelationInfo(diagnosticName("V"))
	beforeProcedure, _ := catalog.ProcedureInfo(diagnosticName("P"))
	beforeDomain, _ := catalog.DomainInfo(diagnosticName("D_AMOUNT"))

	// Mutate the original descriptors after the snapshot was captured.
	cache.ColumnsWithParent["\tT"][0].Type = "MUTATED"
	view.Columns[0].Type = "MUTATED"
	procedure.InputParameters[0].Type = "MUTATED"
	procedure.Name = "MUTATED"
	domain.Type = "MUTATED"

	afterTable, _ := catalog.RelationInfo(diagnosticName("T"))
	afterView, _ := catalog.RelationInfo(diagnosticName("V"))
	afterProcedure, _ := catalog.ProcedureInfo(diagnosticName("P"))
	afterDomain, _ := catalog.DomainInfo(diagnosticName("D_AMOUNT"))

	if afterTable.Columns[0].Type != beforeTable.Columns[0].Type {
		t.Fatalf("table snapshot changed after source mutation: %+v -> %+v", beforeTable, afterTable)
	}
	if afterView.Columns[0].Type != beforeView.Columns[0].Type {
		t.Fatalf("view snapshot changed after source mutation: %+v -> %+v", beforeView, afterView)
	}
	if afterProcedure.Inputs[0].Type != beforeProcedure.Inputs[0].Type {
		t.Fatalf("procedure snapshot changed after source mutation: %+v -> %+v", beforeProcedure, afterProcedure)
	}
	if afterDomain.Type != beforeDomain.Type {
		t.Fatalf("domain snapshot changed after source mutation: %+v -> %+v", beforeDomain, afterDomain)
	}
}

var (
	_ sqlsymbol.SemanticCatalog  = (*diagnosticCatalog)(nil)
	_ sqlsymbol.UniqueKeyCatalog = (*diagnosticCatalog)(nil)
)
