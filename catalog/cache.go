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
	// ColumnsByID retains each relation's columns under the relation's exact
	// catalog identity — ObjectID{Kind: ObjectKindTable, Name: <the
	// relation's exact catalog name>}.Key() — never the shared upper-cased
	// key ColumnsWithParent/ColumnDatabaseKey uses. Two exact-identity
	// relations that fold to the same case (for example a quoted "Foo"
	// alongside FOO) never merge here, unlike ColumnsWithParent, where they
	// silently share one entry. Nil/absent means the active repository has
	// not populated exact-identity column storage yet, not that no relation
	// has columns.
	ColumnsByID       map[string][]*ColumnDesc
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

// ColumnDescsByID resolves a relation's columns by its exact identity (see
// Cache.ColumnsByID), never through ColumnDescs/ColumnDatabase's shared
// upper-cased key: two exact-identity relations that fold to the same case
// never merge here, unlike those accessors. It returns false when
// exact-identity column storage has not been populated yet, or genuinely has
// no such relation.
func (dc *Cache) ColumnDescsByID(name string) (cols []*ColumnDesc, ok bool) {
	if dc == nil {
		return nil, false
	}
	cols, ok = dc.ColumnsByID[ObjectID{Kind: ObjectKindTable, Name: name}.Key()]
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

// RebuildObjects rebuilds Objects from every entry currently in the *ByID
// maps, using fold to normalize an unquoted name (see NewObjectIndex; a nil
// fold uses the default upper-case convention). Call it after replacing any
// one category's *ByID map so Objects reflects every kind loaded so far,
// not just the category that just changed.
func (cc *CatalogCache) RebuildObjects(fold func(string) string) {
	if cc == nil {
		return
	}
	idx := NewObjectIndex(fold)
	for _, v := range cc.ViewsByID {
		idx.Insert(ObjectID{Kind: ObjectKindView, Name: v.Name})
	}
	for _, v := range cc.ProceduresByID {
		idx.Insert(ObjectID{Kind: ObjectKindProcedure, Name: v.Name})
	}
	for _, v := range cc.GeneratorsByID {
		idx.Insert(ObjectID{Kind: ObjectKindGenerator, Name: v.Name})
	}
	for _, v := range cc.DomainsByID {
		idx.Insert(ObjectID{Kind: ObjectKindDomain, Name: v.Name})
	}
	for _, v := range cc.FunctionsByID {
		idx.Insert(ObjectID{Kind: ObjectKindFunction, Name: v.Name})
	}
	for _, v := range cc.IndexesByID {
		idx.Insert(ObjectID{Kind: ObjectKindIndex, Name: v.Name})
	}
	for _, v := range cc.TriggersByID {
		idx.Insert(ObjectID{Kind: ObjectKindTrigger, Name: v.Name})
	}
	cc.Objects = idx
}

// objectIDKeyFor builds the exact ObjectID.Key() a *ByID map entry for name
// under kind is stored at. It exists so insertion (in engine adapters) and
// lookup (here and in consumers) always derive the same key from the same
// inputs.
func objectIDKeyFor(kind ObjectKind, name string) string {
	return ObjectID{Kind: kind, Name: name}.Key()
}

// ViewByID, ProcedureByID, GeneratorByID, DomainByID, FunctionByID,
// IndexByID and TriggerByID resolve a catalog object by its exact name,
// never subject to the upper-casing collision the corresponding legacy
// accessor (View, Procedure, ...) has. They return false when the category
// has not published yet, or genuinely has no such exact object.
func (dc *Cache) ViewByID(name string) (*ViewDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.ViewsByID[objectIDKeyFor(ObjectKindView, name)]
	return v, ok
}

func (dc *Cache) ProcedureByID(name string) (*ProcedureDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.ProceduresByID[objectIDKeyFor(ObjectKindProcedure, name)]
	return v, ok
}

func (dc *Cache) GeneratorByID(name string) (*GeneratorDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.GeneratorsByID[objectIDKeyFor(ObjectKindGenerator, name)]
	return v, ok
}

func (dc *Cache) DomainByID(name string) (*DomainDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.DomainsByID[objectIDKeyFor(ObjectKindDomain, name)]
	return v, ok
}

func (dc *Cache) FunctionByID(name string) (*FunctionDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.FunctionsByID[objectIDKeyFor(ObjectKindFunction, name)]
	return v, ok
}

func (dc *Cache) IndexByID(name string) (*IndexDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.IndexesByID[objectIDKeyFor(ObjectKindIndex, name)]
	return v, ok
}

func (dc *Cache) TriggerByID(name string) (*TriggerDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	v, ok := dc.Catalog.TriggersByID[objectIDKeyFor(ObjectKindTrigger, name)]
	return v, ok
}

// ResolveObjectName resolves want (an exact or unquoted name) against
// Objects for kind. An exact identity match (Kind, want) is returned
// unconditionally, even if it also happens to fold-collide with another
// object: exact identity is never weaker than the unquoted fallback.
// Otherwise a unique folded match resolves; zero matches reports !found,
// and more than one match is returned so the caller can report the
// ambiguity explicitly instead of picking one arbitrarily.
func (dc *Cache) ResolveObjectName(kind ObjectKind, want string) (name string, found bool, ambiguous []ObjectID) {
	if !dc.HasCatalog() || dc.Catalog.Objects == nil {
		return "", false, nil
	}
	if id, ok := dc.Catalog.Objects.Lookup(ObjectID{Kind: kind, Name: want}); ok {
		return id.Name, true, nil
	}
	matches := dc.Catalog.Objects.LookupUnquoted(kind, want)
	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0].Name, true, nil
	default:
		return "", false, matches
	}
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
