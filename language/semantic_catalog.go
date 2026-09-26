package language

import (
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// snapshotSemanticCatalog is a transient view over one immutable metadata
// snapshot. It keeps only the snapshot pointer; descriptor facts are
// projected on demand and never copied into a second retained catalog.
type snapshotSemanticCatalog struct {
	snapshot *catalog.MetadataSnapshot
}

var _ sqlsymbol.SemanticCatalog = snapshotSemanticCatalog{}

func categoryReady(s *catalog.MetadataSnapshot, kind catalog.MetadataKind) bool {
	if s == nil || s.Stale || s.Cache == nil {
		return false
	}
	if status, ok := s.Status[kind]; ok {
		return status.State == catalog.MetadataReady
	}
	return s.Cache.MetadataReady(kind)
}

func namespaceReady(s *catalog.MetadataSnapshot, kinds ...catalog.MetadataKind) bool {
	for _, kind := range kinds {
		if !categoryReady(s, kind) {
			return false
		}
	}
	return true
}

func diagnosticSystemName(name sqlsymbol.Name) bool {
	key := name.Key()
	return len(key) >= 4 && (key[:4] == "RDB$" || key[:4] == "MON$")
}

func nullabilityFromCatalog(valid, nullable bool) sqlsymbol.Nullability {
	if !valid {
		return sqlsymbol.NullUnknown
	}
	if nullable {
		return sqlsymbol.Nullable
	}
	return sqlsymbol.NotNullable
}

func columnFact(desc *catalog.ColumnDesc) sqlsymbol.ColumnFact {
	if desc == nil {
		return sqlsymbol.ColumnFact{}
	}
	return sqlsymbol.ColumnFact{Name: desc.Name, Type: desc.Type, Nullability: nullabilityFromNullString(desc.Null)}
}

func nullabilityFromNullString(value string) sqlsymbol.Nullability {
	switch value {
	case "YES":
		return sqlsymbol.Nullable
	case "NO":
		return sqlsymbol.NotNullable
	default:
		return sqlsymbol.NullUnknown
	}
}

func (c snapshotSemanticCatalog) Columns(name sqlsymbol.Name) ([]sqlsymbol.ColumnType, bool) {
	s := c.snapshot
	if s == nil || s.Stale || s.Cache == nil || !s.Cache.ColumnsReady() {
		return nil, false
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name.Key()}.Key()
	if descriptions, ok := s.Cache.ColumnsByID[key]; ok {
		return diagnosticColumns(descriptions), true
	}
	// Legacy descriptors only prove a match when their own exact spelling is
	// unambiguous. A folded accessor is never used to establish identity.
	descriptions, ok := s.Cache.ColumnDescs(name.Key())
	if !ok || !legacyColumnsMatch(descriptions, name) {
		return nil, false
	}
	return diagnosticColumns(descriptions), true
}

func diagnosticColumns(descriptions []*catalog.ColumnDesc) []sqlsymbol.ColumnType {
	columns := make([]sqlsymbol.ColumnType, 0, len(descriptions))
	for _, desc := range descriptions {
		if desc != nil {
			columns = append(columns, sqlsymbol.ColumnType{Name: desc.Name, Type: desc.Type})
		}
	}
	return columns
}

func legacyColumnsMatch(descriptions []*catalog.ColumnDesc, name sqlsymbol.Name) bool {
	if len(descriptions) == 0 {
		return false
	}
	for _, desc := range descriptions {
		if desc == nil {
			continue
		}
		if name.Quoted && desc.Table != name.Text {
			return false
		}
		if !name.Quoted && name.Key() != desc.Table {
			return false
		}
	}
	return true
}

func (c snapshotSemanticCatalog) UniqueKeys(name sqlsymbol.Name) ([][]string, bool) {
	cache := cacheFor(c.snapshot)
	if cache == nil || cache.Catalog == nil || !namespaceReady(c.snapshot, catalog.MetadataViews, catalog.MetadataIndexes) || !cache.ColumnsReady() {
		return nil, false
	}
	if view, ok := exactView(cache, name); ok && view != nil {
		return nil, false
	}
	columns, ok := c.Columns(name)
	if !ok {
		return nil, false
	}
	var keys [][]string
	for _, index := range indexesForName(cache, name) {
		if index == nil || index.RelationName != name.Key() {
			continue
		}
		if !index.Unique.Valid || !index.Active.Valid || index.Expression.Valid || len(index.Columns) == 0 {
			return nil, false
		}
		if !index.Unique.Bool || !index.Active.Bool {
			continue
		}
		for _, segment := range index.Columns {
			count := 0
			for _, column := range columns {
				if column.Name == segment {
					count++
				}
			}
			if count != 1 {
				return nil, false
			}
		}
		keys = append(keys, append([]string(nil), index.Columns...))
	}
	return keys, true
}

func cacheFor(s *catalog.MetadataSnapshot) *catalog.Cache {
	if s == nil || s.Stale {
		return nil
	}
	return s.Cache
}

func exactView(cache *catalog.Cache, name sqlsymbol.Name) (*catalog.ViewDesc, bool) {
	if cache == nil || cache.Catalog == nil {
		return nil, false
	}
	if view, ok := cache.Catalog.ViewsByID[catalog.ObjectID{Kind: catalog.ObjectKindView, Name: name.Key()}.Key()]; ok {
		return view, true
	}
	if name.Quoted {
		return nil, false
	}
	view, ok := cache.Catalog.Views[catalog.CatalogCacheKey(name.Key())]
	if ok && view != nil && view.Name == name.Key() {
		return view, true
	}
	return nil, false
}

func indexesForName(cache *catalog.Cache, name sqlsymbol.Name) []*catalog.IndexDesc {
	if cache == nil || cache.Catalog == nil {
		return nil
	}
	if name.Quoted {
		var result []*catalog.IndexDesc
		for _, index := range cache.Catalog.IndexesByID {
			if index != nil && index.RelationName == name.Key() {
				result = append(result, index)
			}
		}
		return result
	}
	return cache.Catalog.IndexesByTable[name.Key()]
}

func (c snapshotSemanticCatalog) RelationInfo(name sqlsymbol.Name) (sqlsymbol.RelationFact, sqlsymbol.Knowledge) {
	s := c.snapshot
	if s == nil || s.Stale || s.Cache == nil || diagnosticSystemName(name) {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	if view, ok := exactView(s.Cache, name); ok && namespaceReady(s, catalog.MetadataViews) {
		if view == nil || view.Columns == nil || !s.Cache.ColumnsReady() {
			return sqlsymbol.RelationFact{ColumnsKnown: false}, sqlsymbol.Present
		}
		columns := make([]sqlsymbol.ColumnFact, 0, len(view.Columns))
		for _, col := range view.Columns {
			if col != nil {
				columns = append(columns, columnFact(col))
			}
		}
		return sqlsymbol.RelationFact{Columns: columns, ColumnsKnown: true}, sqlsymbol.Present
	}
	if exactTable(s.Cache, name) && (categoryReady(s, catalog.MetadataRelations) || s.Cache.ColumnsReady()) {
		fact := sqlsymbol.RelationFact{}
		if s.Cache.ColumnsReady() {
			if descriptions, ok := exactTableColumns(s.Cache, name); ok {
				fact.ColumnsKnown = true
				fact.Columns = make([]sqlsymbol.ColumnFact, 0, len(descriptions))
				for _, col := range descriptions {
					if col != nil {
						fact.Columns = append(fact.Columns, columnFact(col))
					}
				}
			}
		}
		return fact, sqlsymbol.Present
	}
	if !namespaceReady(s, catalog.MetadataRelations, catalog.MetadataViews, catalog.MetadataProcedures) {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	if _, proc := c.ProcedureInfo(name); proc == sqlsymbol.Present {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	return sqlsymbol.RelationFact{}, sqlsymbol.Missing
}

func exactTable(cache *catalog.Cache, name sqlsymbol.Name) bool {
	if cache == nil {
		return false
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name.Key()}.Key()
	if _, ok := cache.ColumnsByID[key]; ok {
		return true
	}
	if !name.Quoted {
		for _, table := range cache.SchemaTables[cache.DefaultSchema] {
			if table == name.Key() {
				return true
			}
		}
	}
	if desc, ok := cache.ColumnDescs(name.Key()); ok && legacyColumnsMatch(desc, name) {
		return true
	}
	return false
}

func exactTableColumns(cache *catalog.Cache, name sqlsymbol.Name) ([]*catalog.ColumnDesc, bool) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name.Key()}.Key()
	if columns, ok := cache.ColumnsByID[key]; ok {
		return columns, true
	}
	columns, ok := cache.ColumnDescs(name.Key())
	return columns, ok && legacyColumnsMatch(columns, name)
}

func (c snapshotSemanticCatalog) ProcedureInfo(name sqlsymbol.Name) (sqlsymbol.ProcedureFact, sqlsymbol.Knowledge) {
	s := c.snapshot
	if s == nil || s.Stale || s.Cache == nil || diagnosticSystemName(name) {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	if !namespaceReady(s, catalog.MetadataProcedures) {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	procedure := exactProcedure(s.Cache, name)
	if procedure == nil {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Missing
	}
	fact := sqlsymbol.ProcedureFact{InputsKnown: true, OutputsKnown: true}
	for _, parameter := range procedure.InputParameters {
		if parameter != nil {
			fact.Inputs = append(fact.Inputs, parameterCatalogFact(parameter))
		}
	}
	for _, parameter := range procedure.OutputParameters {
		if parameter != nil {
			fact.Outputs = append(fact.Outputs, parameterCatalogFact(parameter))
		}
	}
	if len(fact.Inputs) == 0 {
		fact.MinInputsKnown = true
	}
	return fact, sqlsymbol.Present
}

func exactProcedure(cache *catalog.Cache, name sqlsymbol.Name) *catalog.ProcedureDesc {
	if cache == nil || cache.Catalog == nil {
		return nil
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: name.Key()}.Key()
	if p, ok := cache.Catalog.ProceduresByID[key]; ok {
		return p
	}
	if name.Quoted {
		return nil
	}
	p := cache.Catalog.Procedures[catalog.CatalogCacheKey(name.Key())]
	if p != nil && p.Name == name.Key() {
		return p
	}
	return nil
}

func parameterCatalogFact(p *catalog.ProcedureParameterDesc) sqlsymbol.ColumnFact {
	fact := sqlsymbol.ColumnFact{Name: p.Name, Type: p.Type, Nullability: nullabilityFromCatalog(p.Nullable.Valid, p.Nullable.Bool)}
	return fact
}

func (c snapshotSemanticCatalog) DomainInfo(name sqlsymbol.Name) (sqlsymbol.DomainFact, sqlsymbol.Knowledge) {
	s := c.snapshot
	if s == nil || s.Stale || s.Cache == nil || diagnosticSystemName(name) {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	if !namespaceReady(s, catalog.MetadataDomains) {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	if s.Cache.Catalog == nil {
		return sqlsymbol.DomainFact{}, sqlsymbol.Missing
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindDomain, Name: name.Key()}.Key()
	domain, ok := s.Cache.Catalog.DomainsByID[key]
	if !ok && !name.Quoted {
		domain = s.Cache.Catalog.Domains[catalog.CatalogCacheKey(name.Key())]
		ok = domain != nil && domain.Name == name.Key()
	}
	if !ok || domain == nil {
		return sqlsymbol.DomainFact{}, sqlsymbol.Missing
	}
	return sqlsymbol.DomainFact{Type: domain.Type, Nullability: nullabilityFromCatalog(domain.Nullable.Valid, domain.Nullable.Bool)}, sqlsymbol.Present
}
