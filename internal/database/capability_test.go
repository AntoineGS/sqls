package database

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

// detailedError stands in for a driver-specific error that carries a
// structured reason. capability.go must reach it through errors.As without
// importing any driver package.
type detailedError struct {
	object  string
	name    string
	feature string
}

func (e *detailedError) Error() string {
	return fmt.Sprintf("test: %s %q: %s", e.object, e.name, e.feature)
}
func (e *detailedError) Unwrap() error { return ErrUnsupportedDDL }
func (e *detailedError) UnsupportedDDLDetail() (string, string, string) {
	return e.object, e.name, e.feature
}

func TestUnsupportedDDLDetail(t *testing.T) {
	detailed := &detailedError{object: "table", name: "ORDERS", feature: `column "TOTAL" is computed`}

	tests := []struct {
		name        string
		err         error
		wantObject  string
		wantName    string
		wantFeature string
		wantOK      bool
	}{
		{
			name:        "a detailed error reports its structure",
			err:         detailed,
			wantObject:  "table",
			wantName:    "ORDERS",
			wantFeature: `column "TOTAL" is computed`,
			wantOK:      true,
		},
		{
			name:        "a wrapped detailed error is still reachable",
			err:         fmt.Errorf("interbase: %w", detailed),
			wantObject:  "table",
			wantName:    "ORDERS",
			wantFeature: `column "TOTAL" is computed`,
			wantOK:      true,
		},
		{name: "a bare sentinel carries no detail", err: ErrUnsupportedDDL},
		{name: "an unrelated error carries no detail", err: errors.New("boom")},
		{name: "a nil error carries no detail", err: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, name, feature, ok := UnsupportedDDLDetail(test.err)
			if ok != test.wantOK {
				t.Fatalf("UnsupportedDDLDetail() ok = %v, want %v", ok, test.wantOK)
			}
			if object != test.wantObject || name != test.wantName || feature != test.wantFeature {
				t.Fatalf("UnsupportedDDLDetail() = (%q, %q, %q), want (%q, %q, %q)",
					object, name, feature, test.wantObject, test.wantName, test.wantFeature)
			}
		})
	}

	// Both the sentinel check and the structured accessor must work on the
	// same error: sub-project 3 branches on the first and renders the second.
	if !errors.Is(detailed, ErrUnsupportedDDL) {
		t.Error("a detailed unsupported-DDL error must satisfy errors.Is(err, ErrUnsupportedDDL)")
	}
	if errors.Is(ErrObjectNotFound, ErrUnsupportedDDL) {
		t.Error("the two sentinels must stay distinct")
	}
}

func TestCapabilityDescriptorShape(t *testing.T) {
	// This test exists so a later edit cannot silently rename or retype a
	// contract field: sub-project 3 compiles against these exact names.
	view := &ViewDesc{
		Schema:      "",
		Name:        "CUSTOMER_VIEW",
		OwnerName:   sql.NullString{String: "SYSDBA", Valid: true},
		ViewSource:  sql.NullString{String: "SELECT 1 FROM RDB$DATABASE", Valid: true},
		Description: sql.NullString{},
		Columns:     []*ColumnDesc{{ColumnBase: ColumnBase{Table: "CUSTOMER_VIEW", Name: "ID"}, Type: "INTEGER"}},
	}
	procedure := &ProcedureDesc{
		Name:   "ADD_CUSTOMER",
		Source: sql.NullString{String: "BEGIN END", Valid: true},
		InputParameters: []*ProcedureParameterDesc{{
			Name:      "EMAIL",
			Position:  0,
			Direction: ParameterInput,
			Type:      "VARCHAR(100)",
			Domain:    "EMAIL_ADDRESS",
			Nullable:  sql.NullBool{Bool: false, Valid: true},
		}},
		OutputParameters: []*ProcedureParameterDesc{{Name: "NEW_ID", Position: 0, Direction: ParameterOutput, Type: "INTEGER"}},
	}
	trigger := &TriggerDesc{
		Name:         "CUSTOMER_BI",
		RelationName: sql.NullString{String: "CUSTOMER", Valid: true},
		Event:        "BEFORE INSERT",
		Sequence:     sql.NullInt64{Int64: 0, Valid: true},
		Active:       sql.NullBool{Bool: true, Valid: true},
	}
	domain := &DomainDesc{
		Name:             "EMAIL_ADDRESS",
		Type:             `VARCHAR(100) CHARACTER SET "UTF8"`,
		Nullable:         sql.NullBool{Bool: false, Valid: true},
		DefaultSource:    sql.NullString{String: "DEFAULT 'a@b'", Valid: true},
		ValidationSource: sql.NullString{String: "CHECK (VALUE LIKE '%@%')", Valid: true},
		CharacterSetName: sql.NullString{String: "UTF8", Valid: true},
		CollationName:    sql.NullString{},
	}
	index := &IndexDesc{
		Name:           "IDX_CUSTOMER_PK",
		RelationName:   "CUSTOMER",
		Columns:        []string{"ID"},
		Expression:     sql.NullString{},
		Unique:         sql.NullBool{Bool: true, Valid: true},
		Active:         sql.NullBool{Bool: true, Valid: true},
		ConstraintName: sql.NullString{String: "PK_CUSTOMER", Valid: true},
	}
	function := &FunctionDesc{
		Name:           "F_LTRIM",
		ReturnType:     "CSTRING(255)",
		ReturnPosition: sql.NullInt64{Int64: 1, Valid: true},
		Arguments:      []*FunctionArgumentDesc{{Name: "F_LTRIM_1", Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "CSTRING(255)"}},
		ModuleName:     sql.NullString{String: "ib_udf", Valid: true},
		EntryPoint:     sql.NullString{String: "IB_LTRIM", Valid: true},
	}
	generator := &GeneratorDesc{Name: "GEN_CUSTOMER_ID", ID: sql.NullInt64{Int64: 1, Valid: true}}

	if view.Columns[0].Name != "ID" || procedure.InputParameters[0].Direction != ParameterInput {
		t.Fatal("descriptor composition is wrong")
	}
	if trigger.Event != "BEFORE INSERT" || domain.CollationName.Valid || !index.Unique.Bool {
		t.Fatal("descriptor fields are wrong")
	}
	if function.Arguments[0].Type != "CSTRING(255)" || !generator.ID.Valid {
		t.Fatal("descriptor fields are wrong")
	}

	wantKinds := []ObjectKind{
		ObjectKindTable, ObjectKindView, ObjectKindProcedure, ObjectKindTrigger,
		ObjectKindDomain, ObjectKindIndex, ObjectKindGenerator, ObjectKindFunction,
	}
	wantText := []string{"table", "view", "procedure", "trigger", "domain", "index", "generator", "function"}
	for i, kind := range wantKinds {
		if string(kind) != wantText[i] {
			t.Errorf("ObjectKind %d = %q, want %q", i, kind, wantText[i])
		}
	}
	if string(ParameterInput) != "input" || string(ParameterOutput) != "output" {
		t.Errorf("ParameterDirection values = (%q, %q), want (\"input\", \"output\")", ParameterInput, ParameterOutput)
	}
}

func TestNonInterBaseRepositoriesDoNotImplementCapabilities(t *testing.T) {
	// The guard that this change stays additive: no other driver's repository
	// may accidentally satisfy a capability, and MockDBRepository must not
	// either, or every existing handler test would start taking the capability
	// branch and panic on a nil func field.
	repositories := map[dialect.DatabaseDriver]DBRepository{
		dialect.DatabaseDriverMySQL:      NewMySQLDBRepository(nil),
		dialect.DatabaseDriverPostgreSQL: NewPostgreSQLDBRepository(nil),
		dialect.DatabaseDriverSQLite3:    NewSQLite3DBRepository(nil),
		dialect.DatabaseDriverMssql:      NewMssqlDBRepository(nil),
		dialect.DatabaseDriverH2:         NewH2DBRepository(nil),
		dialect.DatabaseDriverVertica:    NewVerticaDBRepository(nil),
		dialect.DatabaseDriverClickhouse: NewClickhouseRepository(nil),
		dialect.DatabaseDriverOracle:     NewOracleDBRepository(nil),
		dialect.DatabaseDriver("mock"):   NewMockDBRepository(nil),
	}

	for driver, repository := range repositories {
		t.Run(string(driver), func(t *testing.T) {
			if _, ok := repository.(CatalogRepository); ok {
				t.Errorf("%s must not implement CatalogRepository", driver)
			}
			if _, ok := repository.(DDLRepository); ok {
				t.Errorf("%s must not implement DDLRepository", driver)
			}
			if _, ok := repository.(ExplainRepository); ok {
				t.Errorf("%s must not implement ExplainRepository", driver)
			}
			if _, ok := repository.(CatalogSnapshotRepository); ok {
				t.Errorf("%s must not implement CatalogSnapshotRepository", driver)
			}
		})
	}
}
