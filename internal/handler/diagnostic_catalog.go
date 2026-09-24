package handler

import (
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// diagnosticSystemPrefixes lists the identifier prefixes sqls treats as
// InterBase system catalog objects. This adapter never enumerates RDB$/MON$
// objects, so it cannot claim completeness over them regardless of how
// complete user-object metadata is.
var diagnosticSystemPrefixes = []string{"RDB$", "MON$"}

func isDiagnosticSystemName(key string) bool {
	for _, prefix := range diagnosticSystemPrefixes {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func nullabilityFromColumnDesc(null string) sqlsymbol.Nullability {
	switch null {
	case "YES":
		return sqlsymbol.Nullable
	case "NO":
		return sqlsymbol.NotNullable
	default:
		return sqlsymbol.NullUnknown
	}
}

// relationsNamespaceReady reports whether every category contributing
// user-visible relations (tables and views) has finished loading. Only then
// can an absent name be proven Missing rather than merely Unknown.
func relationsNamespaceReady(cache *database.DBCache) bool {
	return cache.MetadataReady(database.MetadataRelations, database.MetadataViews)
}

func (c *diagnosticCatalog) RelationInfo(table sqlsymbol.Name) (sqlsymbol.RelationFact, sqlsymbol.Knowledge) {
	if c == nil {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	key := table.Key()
	if isDiagnosticSystemName(key) {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	if fact, ok := c.relations[key]; ok {
		return sqlsymbol.RelationFact{
			Columns:      append([]sqlsymbol.ColumnFact(nil), fact.Columns...),
			ColumnsKnown: fact.ColumnsKnown,
		}, sqlsymbol.Present
	}
	if c.relationsKnown {
		return sqlsymbol.RelationFact{}, sqlsymbol.Missing
	}
	return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
}

func (c *diagnosticCatalog) ProcedureInfo(name sqlsymbol.Name) (sqlsymbol.ProcedureFact, sqlsymbol.Knowledge) {
	if c == nil {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	key := name.Key()
	if isDiagnosticSystemName(key) {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	if fact, ok := c.procedures[key]; ok {
		return sqlsymbol.ProcedureFact{
			Inputs:         append([]sqlsymbol.ColumnFact(nil), fact.Inputs...),
			Outputs:        append([]sqlsymbol.ColumnFact(nil), fact.Outputs...),
			InputsKnown:    fact.InputsKnown,
			OutputsKnown:   fact.OutputsKnown,
			MinInputs:      fact.MinInputs,
			MinInputsKnown: fact.MinInputsKnown,
		}, sqlsymbol.Present
	}
	if c.proceduresKnown {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Missing
	}
	return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
}

func (c *diagnosticCatalog) DomainInfo(name sqlsymbol.Name) (sqlsymbol.DomainFact, sqlsymbol.Knowledge) {
	if c == nil {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	key := name.Key()
	if isDiagnosticSystemName(key) {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	if fact, ok := c.domains[key]; ok {
		return fact, sqlsymbol.Present
	}
	if c.domainsKnown {
		return sqlsymbol.DomainFact{}, sqlsymbol.Missing
	}
	return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
}

// buildDiagnosticRelations copies every relation the cache currently knows
// about, tables and views alike, whether or not the categories that enumerate
// them have finished loading. A single relation's own existence and columns
// are available independently of the whole namespace being provably complete;
// relationsNamespaceReady (not this function) decides whether an absent name
// can be proven Missing.
func buildDiagnosticRelations(cache *database.DBCache) map[string]sqlsymbol.RelationFact {
	relations := make(map[string]sqlsymbol.RelationFact)
	for _, table := range cache.SortedTables() {
		relations[table] = relationFactForTable(cache, table)
	}
	if cache.HasCatalog() {
		for _, view := range cache.Catalog.Views {
			if view == nil {
				continue
			}
			relations[view.Name] = relationFactForView(view)
		}
	}
	return relations
}

func relationFactForTable(cache *database.DBCache, table string) sqlsymbol.RelationFact {
	descriptions, ok := cache.ColumnDescs(table)
	if !ok || !cache.ColumnsReady() {
		return sqlsymbol.RelationFact{ColumnsKnown: false}
	}
	columns := make([]sqlsymbol.ColumnFact, 0, len(descriptions))
	for _, description := range descriptions {
		if description == nil {
			continue
		}
		columns = append(columns, sqlsymbol.ColumnFact{
			Name:        description.Name,
			Type:        description.Type,
			Nullability: nullabilityFromColumnDesc(description.Null),
		})
	}
	return sqlsymbol.RelationFact{Columns: columns, ColumnsKnown: true}
}

func relationFactForView(view *database.ViewDesc) sqlsymbol.RelationFact {
	if view.Columns == nil {
		return sqlsymbol.RelationFact{ColumnsKnown: false}
	}
	columns := make([]sqlsymbol.ColumnFact, 0, len(view.Columns))
	for _, description := range view.Columns {
		if description == nil {
			continue
		}
		columns = append(columns, sqlsymbol.ColumnFact{
			Name:        description.Name,
			Type:        description.Type,
			Nullability: nullabilityFromColumnDesc(description.Null),
		})
	}
	return sqlsymbol.RelationFact{Columns: columns, ColumnsKnown: true}
}

func buildDiagnosticProcedures(cache *database.DBCache) map[string]sqlsymbol.ProcedureFact {
	procedures := make(map[string]sqlsymbol.ProcedureFact)
	if !cache.HasCatalog() || !cache.MetadataReady(database.MetadataProcedures) {
		return procedures
	}
	for _, procedure := range cache.Catalog.Procedures {
		if procedure == nil {
			continue
		}
		procedures[procedure.Name] = procedureFactFor(procedure)
	}
	return procedures
}

func procedureFactFor(procedure *database.ProcedureDesc) sqlsymbol.ProcedureFact {
	inputs := make([]sqlsymbol.ColumnFact, len(procedure.InputParameters))
	for i, parameter := range procedure.InputParameters {
		inputs[i] = parameterFact(parameter)
	}
	outputs := make([]sqlsymbol.ColumnFact, len(procedure.OutputParameters))
	for i, parameter := range procedure.OutputParameters {
		outputs[i] = parameterFact(parameter)
	}
	fact := sqlsymbol.ProcedureFact{
		Inputs:       inputs,
		Outputs:      outputs,
		InputsKnown:  true,
		OutputsKnown: true,
	}
	// A zero-parameter procedure has a trivially known minimum; the catalog
	// otherwise cannot distinguish required from defaulted trailing
	// parameters, so it must not claim a minimum count.
	if len(procedure.InputParameters) == 0 {
		fact.MinInputs = 0
		fact.MinInputsKnown = true
	}
	return fact
}

func parameterFact(parameter *database.ProcedureParameterDesc) sqlsymbol.ColumnFact {
	fact := sqlsymbol.ColumnFact{Name: parameter.Name, Type: parameter.Type}
	if parameter.Nullable.Valid {
		if parameter.Nullable.Bool {
			fact.Nullability = sqlsymbol.Nullable
		} else {
			fact.Nullability = sqlsymbol.NotNullable
		}
	}
	return fact
}

func buildDiagnosticDomains(cache *database.DBCache) map[string]sqlsymbol.DomainFact {
	domains := make(map[string]sqlsymbol.DomainFact)
	if !cache.HasCatalog() || !cache.MetadataReady(database.MetadataDomains) {
		return domains
	}
	for _, domain := range cache.Catalog.Domains {
		if domain == nil {
			continue
		}
		fact := sqlsymbol.DomainFact{Type: domain.Type}
		if domain.Nullable.Valid {
			if domain.Nullable.Bool {
				fact.Nullability = sqlsymbol.Nullable
			} else {
				fact.Nullability = sqlsymbol.NotNullable
			}
		}
		domains[domain.Name] = fact
	}
	return domains
}
