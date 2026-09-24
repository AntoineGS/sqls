package catalog

import (
	"sort"
	"strings"
)

// Cache is the immutable-by-contract published view of one metadata
// generation's catalog data.
type Cache struct {
	DefaultSchema     string
	Schemas           map[string]string
	SchemaTables      map[string][]string
	ColumnsWithParent map[string][]*ColumnDesc
	ForeignKeys       map[string]map[string][]*ForeignKey
	Metadata          map[MetadataKind]MetadataState
	PrimaryKeyColumns map[string]map[string]struct{}
	// Catalog holds extended catalog objects. It is nil when the active
	// repository does not implement CatalogRepository, and also before the
	// first successful secondary pass.
	Catalog *CatalogCache
}

// MetadataReady reports whether every explicitly requested category completed.
// An absent category is unknown; empty result maps do not imply readiness.
func (dc *Cache) MetadataReady(kinds ...MetadataKind) bool {
	if dc == nil || len(kinds) == 0 {
		return false
	}
	for _, kind := range kinds {
		if dc.Metadata[kind] != MetadataReady {
			return false
		}
	}
	return true
}

// ColumnsReady reports whether either supported column view has completed.
func (dc *Cache) ColumnsReady() bool {
	return dc != nil && (dc.MetadataReady(MetadataColumnsCurrent) || dc.MetadataReady(MetadataColumnsAll))
}

func (dc *Cache) Database(dbName string) (db string, ok bool) {
	db, ok = dc.Schemas[strings.ToUpper(dbName)]
	return
}

func (dc *Cache) SortedSchemas() []string {
	dbs := []string{}
	for _, db := range dc.Schemas {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	return dbs
}

func (dc *Cache) SortedTablesByDBName(dbName string) (tbls []string, ok bool) {
	tbls, ok = dc.SchemaTables[strings.ToUpper(dbName)]
	tbls = append([]string(nil), tbls...)
	sort.Strings(tbls)
	return
}

func (dc *Cache) SortedTables() []string {
	tbls, _ := dc.SortedTablesByDBName(dc.DefaultSchema)
	return tbls
}

func (dc *Cache) ColumnDescs(tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[ColumnDatabaseKey(dc.DefaultSchema, tableName)]
	return
}

func (dc *Cache) ColumnDatabase(dbName, tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[ColumnDatabaseKey(dbName, tableName)]
	return
}

func (dc *Cache) Column(tableName, colName string) (*ColumnDesc, bool) {
	cols, ok := dc.ColumnsWithParent[ColumnDatabaseKey(dc.DefaultSchema, tableName)]
	if !ok {
		return nil, false
	}
	for _, col := range cols {
		if strings.EqualFold(col.Name, colName) {
			return col, true
		}
	}
	return nil, false
}

// HasCatalog reports whether the active repository produced an extended
// catalog. It is the single gate a feature uses before touching catalog data.
func (dc *Cache) HasCatalog() bool {
	return dc != nil && dc.Catalog != nil
}

func (dc *Cache) View(name string) (*ViewDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	view, ok := dc.Catalog.Views[CatalogCacheKey(name)]
	return view, ok
}

func (dc *Cache) Procedure(name string) (*ProcedureDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	procedure, ok := dc.Catalog.Procedures[CatalogCacheKey(name)]
	return procedure, ok
}

func (dc *Cache) Generator(name string) (*GeneratorDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	generator, ok := dc.Catalog.Generators[CatalogCacheKey(name)]
	return generator, ok
}

func (dc *Cache) Domain(name string) (*DomainDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	domain, ok := dc.Catalog.Domains[CatalogCacheKey(name)]
	return domain, ok
}

func (dc *Cache) Function(name string) (*FunctionDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	function, ok := dc.Catalog.Functions[CatalogCacheKey(name)]
	return function, ok
}

func (dc *Cache) Index(name string) (*IndexDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	index, ok := dc.Catalog.Indexes[CatalogCacheKey(name)]
	return index, ok
}

func (dc *Cache) Trigger(name string) (*TriggerDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	trigger, ok := dc.Catalog.Triggers[CatalogCacheKey(name)]
	return trigger, ok
}

func (dc *Cache) IndexesForTable(table string) []*IndexDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.IndexesByTable[CatalogCacheKey(table)]
}

func (dc *Cache) TriggersForTable(table string) []*TriggerDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.TriggersByTable[CatalogCacheKey(table)]
}

func (dc *Cache) SortedProcedures() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Procedures))
	for _, procedure := range dc.Catalog.Procedures {
		names = append(names, procedure.Name)
	}
	sort.Strings(names)
	return names
}

func (dc *Cache) SortedViews() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Views))
	for _, view := range dc.Catalog.Views {
		names = append(names, view.Name)
	}
	sort.Strings(names)
	return names
}

func (dc *Cache) SortedGenerators() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Generators))
	for _, generator := range dc.Catalog.Generators {
		names = append(names, generator.Name)
	}
	sort.Strings(names)
	return names
}

// SortedFunctions exists because external functions are the one object kind a
// consumer must enumerate rather than look up.
func (dc *Cache) SortedFunctions() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Functions))
	for _, function := range dc.Catalog.Functions {
		names = append(names, function.Name)
	}
	sort.Strings(names)
	return names
}
