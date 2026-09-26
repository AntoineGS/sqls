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

func columnsReady(s *catalog.MetadataSnapshot) bool {
	return categoryReady(s, catalog.MetadataColumnsCurrent) || categoryReady(s, catalog.MetadataColumnsAll)
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
	if s == nil || s.Stale || s.Cache == nil {
		return nil, false
	}
	if categoryReady(s, catalog.MetadataViews) {
		view, found, _ := exactView(s.Cache, name)
		if found {
			if view == nil || view.Columns == nil {
				return nil, false
			}
			return diagnosticColumns(view.Columns), true
		}
	}
	if !columnsReady(s) || !exactBaseTable(s, name) {
		return nil, false
	}
	descriptions, ok := exactTableColumns(s.Cache, name)
	if !ok {
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
	matched := false
	for _, desc := range descriptions {
		if desc == nil {
			continue
		}
		matched = true
		if name.Quoted && desc.Table != name.Text {
			return false
		}
		if !name.Quoted && name.Key() != desc.Table {
			return false
		}
	}
	return matched
}

func (c snapshotSemanticCatalog) UniqueKeys(name sqlsymbol.Name) ([][]string, bool) {
	cache := cacheFor(c.snapshot)
	if cache == nil || cache.Catalog == nil || !namespaceReady(c.snapshot, catalog.MetadataViews, catalog.MetadataIndexes) || !columnsReady(c.snapshot) {
		return nil, false
	}
	if _, found, ambiguous := exactView(cache, name); ambiguous || found {
		return nil, false
	}
	columns, ok := c.Columns(name)
	if !ok {
		return nil, false
	}
	keys := make([][]string, 0)
	for _, index := range indexesForName(cache, name) {
		if index == nil || index.RelationName != name.Key() {
			continue
		}
		if !index.Unique.Valid {
			return nil, false
		}
		if !index.Unique.Bool {
			continue
		}
		if !index.Active.Valid {
			return nil, false
		}
		if !index.Active.Bool {
			continue
		}
		if index.Expression.Valid || len(index.Columns) == 0 {
			return nil, false
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

func exactView(cache *catalog.Cache, name sqlsymbol.Name) (*catalog.ViewDesc, bool, bool) {
	if cache == nil || cache.Catalog == nil {
		return nil, false, false
	}
	if view, ok := cache.Catalog.ViewsByID[catalog.ObjectID{Kind: catalog.ObjectKindView, Name: name.Key()}.Key()]; ok && view != nil {
		if view.Name == name.Key() {
			return view, true, false
		}
		return nil, false, true
	}
	folded, ok := cache.Catalog.Views[catalog.CatalogCacheKey(name.Key())]
	if !ok || folded == nil {
		return nil, false, false
	}
	if folded.Name == name.Key() {
		return folded, true, false
	}
	// A folded legacy entry with different spelling may be a colliding
	// quoted view that the legacy map cannot represent independently.
	if name.Quoted {
		return nil, false, true
	}
	return nil, false, false
}

func indexesForName(cache *catalog.Cache, name sqlsymbol.Name) []*catalog.IndexDesc {
	if cache == nil || cache.Catalog == nil {
		return nil
	}
	// Merge the exact map and legacy maps for this one relation only. The
	// category status establishes coverage; exact RelationName filtering
	// prevents folded table buckets from lending a neighbor's indexes.
	seen := make(map[*catalog.IndexDesc]struct{})
	var result []*catalog.IndexDesc
	add := func(index *catalog.IndexDesc) {
		if index == nil || index.RelationName != name.Key() {
			return
		}
		if _, ok := seen[index]; ok {
			return
		}
		seen[index] = struct{}{}
		result = append(result, index)
	}
	for _, index := range cache.Catalog.IndexesByID {
		add(index)
	}
	for _, index := range cache.Catalog.IndexesByTable[catalog.CatalogCacheKey(name.Key())] {
		add(index)
	}
	for _, index := range cache.Catalog.Indexes {
		add(index)
	}
	return result
}

func (c snapshotSemanticCatalog) RelationInfo(name sqlsymbol.Name) (sqlsymbol.RelationFact, sqlsymbol.Knowledge) {
	s := c.snapshot
	if s == nil || s.Stale || s.Cache == nil || diagnosticSystemName(name) {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	viewAmbiguous := false
	if namespaceReady(s, catalog.MetadataViews) {
		view, found, ambiguous := exactView(s.Cache, name)
		viewAmbiguous = ambiguous
		if found {
			if view.Columns == nil {
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
		if ambiguous && exactRelationListed(s.Cache, name) && categoryReady(s, catalog.MetadataRelations) {
			return sqlsymbol.RelationFact{ColumnsKnown: false}, sqlsymbol.Present
		}
	}
	if viewAmbiguous {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	if exactRelationListed(s.Cache, name) && categoryReady(s, catalog.MetadataRelations) {
		fact := sqlsymbol.RelationFact{}
		// SchemaTables contains both base tables and views. Do not interpret
		// the columns-by-ID entries as table proof until the complete views
		// category classifies this exact relation name as a base table.
		if exactBaseTable(s, name) && columnsReady(s) {
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
		// Selectable procedures can appear in relation positions even though
		// they do not have table columns. Preserve their exact identity as a
		// present relation instead of degrading a known object to Unknown.
		return sqlsymbol.RelationFact{}, sqlsymbol.Present
	} else if proc == sqlsymbol.Unknown {
		return sqlsymbol.RelationFact{}, sqlsymbol.Unknown
	}
	return sqlsymbol.RelationFact{}, sqlsymbol.Missing
}

func exactRelationListed(cache *catalog.Cache, name sqlsymbol.Name) bool {
	if cache == nil {
		return false
	}
	for _, relation := range cache.SchemaTables[cache.DefaultSchema] {
		if relation == name.Key() {
			return true
		}
	}
	return false
}

func exactBaseTable(s *catalog.MetadataSnapshot, name sqlsymbol.Name) bool {
	if s == nil || s.Cache == nil || s.Cache.Catalog == nil || !categoryReady(s, catalog.MetadataRelations) || !categoryReady(s, catalog.MetadataViews) || !exactRelationListed(s.Cache, name) {
		return false
	}
	_, found, ambiguous := exactView(s.Cache, name)
	return !found && !ambiguous
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
	if s.Cache.Catalog == nil {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	procedure, found, ambiguous := exactProcedure(s.Cache, name)
	if ambiguous {
		return sqlsymbol.ProcedureFact{}, sqlsymbol.Unknown
	}
	if !found || procedure == nil {
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

func exactProcedure(cache *catalog.Cache, name sqlsymbol.Name) (*catalog.ProcedureDesc, bool, bool) {
	if cache == nil || cache.Catalog == nil {
		return nil, false, false
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: name.Key()}.Key()
	if p, ok := cache.Catalog.ProceduresByID[key]; ok && p != nil {
		if p.Name == name.Key() {
			return p, true, false
		}
		return nil, false, true
	}
	p, ok := cache.Catalog.Procedures[catalog.CatalogCacheKey(name.Key())]
	if !ok || p == nil {
		return nil, false, false
	}
	if p.Name == name.Key() {
		return p, true, false
	}
	return nil, false, name.Quoted
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
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	key := catalog.ObjectID{Kind: catalog.ObjectKindDomain, Name: name.Key()}.Key()
	domain, ok := s.Cache.Catalog.DomainsByID[key]
	if ok && domain == nil {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	if ok && domain.Name != name.Key() {
		return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
	}
	if !ok {
		domain = s.Cache.Catalog.Domains[catalog.CatalogCacheKey(name.Key())]
		if domain != nil {
			if domain.Name == name.Key() {
				ok = true
			} else if name.Quoted {
				return sqlsymbol.DomainFact{}, sqlsymbol.Unknown
			}
		}
	}
	if !ok || domain == nil {
		return sqlsymbol.DomainFact{}, sqlsymbol.Missing
	}
	return sqlsymbol.DomainFact{Type: domain.Type, Nullability: nullabilityFromCatalog(domain.Nullable.Valid, domain.Nullable.Bool)}, sqlsymbol.Present
}
