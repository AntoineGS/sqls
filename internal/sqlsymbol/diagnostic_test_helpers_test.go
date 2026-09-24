package sqlsymbol

import (
	"strings"
	"testing"
)

// requireCodeCount fails the test unless text produces exactly want findings
// with code under c.
func requireCodeCount(t *testing.T, text string, c Catalog, code string, want int) {
	t.Helper()
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, f := range a.Diagnostics(c) {
		if f.Code == code {
			count++
		}
	}
	if count != want {
		t.Fatalf("%s: got %d, want %d", code, count, want)
	}
}

// isDiagnosticFixtureSystemName mirrors the production adapter's rule: sqls
// never claims completeness over system catalog objects, regardless of how
// complete its user-object fixtures are.
func isDiagnosticFixtureSystemName(key string) bool {
	return strings.HasPrefix(key, "RDB$") || strings.HasPrefix(key, "MON$")
}

var (
	_ SemanticCatalog  = (*diagnosticFixtureCatalog)(nil)
	_ UniqueKeyCatalog = (*diagnosticFixtureCatalog)(nil)
)

// diagnosticFixtureCatalog is a SemanticCatalog test double whose completeness
// is controlled per user-object category (relationsKnown, proceduresKnown,
// domainsKnown) and, for relations, independently per relation's column list
// (RelationFact.ColumnsKnown).
type diagnosticFixtureCatalog struct {
	relations       map[string]RelationFact
	relationsKnown  bool
	procedures      map[string]ProcedureFact
	proceduresKnown bool
	domains         map[string]DomainFact
	domainsKnown    bool
	keys            map[string][][]string
}

// newDiagnosticFixtureCatalog returns a complete user-object catalog with
// table T (ID INTEGER NOT NULL, V INTEGER NULL, NAME VARCHAR(10), N SMALLINT),
// table U (ID INTEGER NOT NULL, V INTEGER NULL), and procedure P (two INTEGER
// inputs, one INTEGER output).
func newDiagnosticFixtureCatalog() *diagnosticFixtureCatalog {
	return &diagnosticFixtureCatalog{
		relationsKnown:  true,
		proceduresKnown: true,
		domainsKnown:    true,
		relations: map[string]RelationFact{
			"T": {
				Columns: []ColumnFact{
					{Name: "ID", Type: "INTEGER", Nullability: NotNullable},
					{Name: "V", Type: "INTEGER", Nullability: Nullable},
					{Name: "NAME", Type: "VARCHAR(10)", Nullability: NullUnknown},
					{Name: "N", Type: "SMALLINT", Nullability: NullUnknown},
				},
				ColumnsKnown: true,
			},
			"U": {
				Columns: []ColumnFact{
					{Name: "ID", Type: "INTEGER", Nullability: NotNullable},
					{Name: "V", Type: "INTEGER", Nullability: Nullable},
				},
				ColumnsKnown: true,
			},
		},
		procedures: map[string]ProcedureFact{
			"P": {
				Inputs: []ColumnFact{
					{Name: "IN1", Type: "INTEGER"},
					{Name: "IN2", Type: "INTEGER"},
				},
				InputsKnown: true,
				Outputs: []ColumnFact{
					{Name: "OUT1", Type: "INTEGER"},
				},
				OutputsKnown:   true,
				MinInputs:      2,
				MinInputsKnown: true,
			},
		},
		domains: map[string]DomainFact{},
		keys:    map[string][][]string{},
	}
}

func (c *diagnosticFixtureCatalog) Columns(table Name) ([]ColumnType, bool) {
	fact, knowledge := c.RelationInfo(table)
	if knowledge != Present || !fact.ColumnsKnown {
		return nil, false
	}
	columns := make([]ColumnType, len(fact.Columns))
	for i, column := range fact.Columns {
		columns[i] = ColumnType{Name: column.Name, Type: column.Type}
	}
	return columns, true
}

func (c *diagnosticFixtureCatalog) UniqueKeys(table Name) ([][]string, bool) {
	keys, ok := c.keys[table.Key()]
	if !ok {
		return nil, false
	}
	result := make([][]string, len(keys))
	for i, key := range keys {
		result[i] = append([]string(nil), key...)
	}
	return result, true
}

func (c *diagnosticFixtureCatalog) RelationInfo(table Name) (RelationFact, Knowledge) {
	key := table.Key()
	if isDiagnosticFixtureSystemName(key) {
		return RelationFact{}, Unknown
	}
	if fact, ok := c.relations[key]; ok {
		return fact, Present
	}
	if c.relationsKnown {
		return RelationFact{}, Missing
	}
	return RelationFact{}, Unknown
}

func (c *diagnosticFixtureCatalog) ProcedureInfo(name Name) (ProcedureFact, Knowledge) {
	key := name.Key()
	if isDiagnosticFixtureSystemName(key) {
		return ProcedureFact{}, Unknown
	}
	if fact, ok := c.procedures[key]; ok {
		return fact, Present
	}
	if c.proceduresKnown {
		return ProcedureFact{}, Missing
	}
	return ProcedureFact{}, Unknown
}

func (c *diagnosticFixtureCatalog) DomainInfo(name Name) (DomainFact, Knowledge) {
	key := name.Key()
	if isDiagnosticFixtureSystemName(key) {
		return DomainFact{}, Unknown
	}
	if fact, ok := c.domains[key]; ok {
		return fact, Present
	}
	if c.domainsKnown {
		return DomainFact{}, Missing
	}
	return DomainFact{}, Unknown
}
