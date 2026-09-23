package database

import (
	"context"
	"log"
	"sort"
	"strings"
)

type DBCacheGenerator struct {
	repo DBRepository
}

func NewDBCacheUpdater(repo DBRepository) *DBCacheGenerator {
	return &DBCacheGenerator{
		repo: repo,
	}
}

// snapshot returns a generator whose reads are served from one consistent
// catalog read, plus the closer that ends it. A repository without the
// capability is used directly with a no-op closer, which is the normal case
// for every driver but InterBase.
func (u *DBCacheGenerator) snapshot(ctx context.Context) (*DBCacheGenerator, func() error) {
	noop := func() error { return nil }
	source, ok := u.repo.(CatalogSnapshotRepository)
	if !ok {
		return u, noop
	}
	repo, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil || repo == nil {
		// A snapshot is an optimisation, not a requirement: log the reason and
		// build the cache the slow way rather than failing the whole pass.
		if closeSnapshot != nil {
			_ = closeSnapshot()
		}
		if err != nil {
			log.Println("db cache: catalog snapshot unavailable:", err)
		}
		return u, noop
	}
	return &DBCacheGenerator{repo: repo}, closeSnapshot
}

func (u *DBCacheGenerator) GenerateDBCachePrimary(ctx context.Context) (*DBCache, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateDBCachePrimary(ctx)
}

func (u *DBCacheGenerator) GenerateDBCacheSecondary(ctx context.Context) (map[string][]*ColumnDesc, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateDBCacheSecondary(ctx)
}

func (u *DBCacheGenerator) generateDBCachePrimary(ctx context.Context) (*DBCache, error) {
	var err error
	dbCache := &DBCache{}
	dbCache.defaultSchema, err = u.repo.CurrentSchema(ctx)
	if err != nil {
		return nil, err
	}
	schemas, err := u.genSchemaCache(ctx)
	if err != nil {
		return nil, err
	}
	dbCache.Schemas = make(map[string]string)
	for index, element := range schemas {
		dbCache.Schemas[strings.ToUpper(index)] = element
	}

	if dbCache.defaultSchema == "" {
		var topKey string
		for k := range dbCache.Schemas {
			topKey = k
			continue
		}
		dbCache.defaultSchema = dbCache.Schemas[topKey]
	}
	schemaTables, err := u.repo.SchemaTables(ctx)
	if err != nil {
		return nil, err
	}
	dbCache.SchemaTables = make(map[string][]string)
	for index, element := range schemaTables {
		dbCache.SchemaTables[strings.ToUpper(index)] = element
	}

	dbCache.ColumnsWithParent, err = u.genColumnCacheCurrent(ctx, dbCache.defaultSchema)
	if err != nil {
		return nil, err
	}
	dbCache.ForeignKeys, err = u.genForeignKeysCache(ctx, dbCache.defaultSchema)
	if err != nil {
		return nil, err
	}
	return dbCache, nil
}

func (u *DBCacheGenerator) generateDBCacheSecondary(ctx context.Context) (map[string][]*ColumnDesc, error) {
	return u.genColumnCacheAll(ctx)
}

func (u *DBCacheGenerator) genSchemaCache(ctx context.Context) (map[string]string, error) {
	dbs, err := u.repo.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	databaseMap := map[string]string{}
	for _, db := range dbs {
		databaseMap[strings.ToUpper(db)] = db
	}
	return databaseMap, nil
}

func (u *DBCacheGenerator) genColumnCacheCurrent(ctx context.Context, schemaName string) (map[string][]*ColumnDesc, error) {
	columnDescs, err := u.repo.DescribeDatabaseTableBySchema(ctx, schemaName)
	if err != nil {
		return nil, err
	}
	return genColumnMap(columnDescs), nil
}

func (u *DBCacheGenerator) genColumnCacheAll(ctx context.Context) (map[string][]*ColumnDesc, error) {
	columnDescs, err := u.repo.DescribeDatabaseTable(ctx)
	if err != nil {
		return nil, err
	}
	return genColumnMap(columnDescs), nil
}

func (u *DBCacheGenerator) genForeignKeysCache(ctx context.Context, schemaName string) (map[string]map[string][]*ForeignKey, error) {
	retVal := make(map[string]map[string][]*ForeignKey)
	fk, err := u.repo.DescribeForeignKeysBySchema(ctx, schemaName)
	if err != nil {
		return nil, err
	}

	for _, cur := range fk {
		elem := (*cur)[0]
		refs, ok := retVal[elem[0].Table]
		if !ok {
			refs = make(map[string][]*ForeignKey)
		}
		refs[elem[1].Table] = append(refs[elem[1].Table], cur)
		retVal[elem[0].Table] = refs

		refs, ok = retVal[elem[1].Table]
		if !ok {
			refs = make(map[string][]*ForeignKey)
		}
		refs[elem[0].Table] = append(refs[elem[0].Table], cur)
		retVal[elem[1].Table] = refs
	}
	return retVal, nil
}

func genColumnMap(columnDescs []*ColumnDesc) map[string][]*ColumnDesc {
	columnMap := map[string][]*ColumnDesc{}
	for _, desc := range columnDescs {
		key := columnDatabaseKey(desc.Schema, desc.Table)
		columnMap[key] = append(columnMap[key], desc)
	}
	return columnMap
}

type DBCache struct {
	defaultSchema     string
	Schemas           map[string]string
	SchemaTables      map[string][]string
	ColumnsWithParent map[string][]*ColumnDesc
	ForeignKeys       map[string]map[string][]*ForeignKey
	// Catalog holds extended catalog objects. It is nil when the active
	// repository does not implement CatalogRepository, and also before the
	// first successful secondary pass.
	Catalog *CatalogCache
}

func (dc *DBCache) Database(dbName string) (db string, ok bool) {
	db, ok = dc.Schemas[strings.ToUpper(dbName)]
	return
}

func (dc *DBCache) SortedSchemas() []string {
	dbs := []string{}
	for _, db := range dc.Schemas {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	return dbs
}

func (dc *DBCache) SortedTablesByDBName(dbName string) (tbls []string, ok bool) {
	tbls, ok = dc.SchemaTables[strings.ToUpper(dbName)]
	tbls = append([]string(nil), tbls...)
	sort.Strings(tbls)
	return
}

func (dc *DBCache) SortedTables() []string {
	tbls, _ := dc.SortedTablesByDBName(dc.defaultSchema)
	return tbls
}

func (dc *DBCache) ColumnDescs(tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[columnDatabaseKey(dc.defaultSchema, tableName)]
	return
}

func (dc *DBCache) ColumnDatabase(dbName, tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[columnDatabaseKey(dbName, tableName)]
	return
}

func (dc *DBCache) Column(tableName, colName string) (*ColumnDesc, bool) {
	cols, ok := dc.ColumnsWithParent[columnDatabaseKey(dc.defaultSchema, tableName)]
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

func columnDatabaseKey(dbName, tableName string) string {
	return strings.ToUpper(dbName) + "\t" + strings.ToUpper(tableName)
}

// CatalogCache holds extended catalog objects. A nil *CatalogCache means the
// active repository does not implement CatalogRepository. All maps are keyed by
// the upper-cased object name.
type CatalogCache struct {
	Views           map[string]*ViewDesc
	Procedures      map[string]*ProcedureDesc
	Generators      map[string]*GeneratorDesc
	Domains         map[string]*DomainDesc
	Functions       map[string]*FunctionDesc
	Indexes         map[string]*IndexDesc
	IndexesByTable  map[string][]*IndexDesc
	Triggers        map[string]*TriggerDesc
	TriggersByTable map[string][]*TriggerDesc
}

// catalogCacheKey normalises an object name for cache lookup. InterBase stores
// catalog names upper-cased while users type them lower-cased, so an
// exact-match accessor would silently miss for every lower-case identifier and
// the failure would look like missing metadata rather than a lookup bug. This
// follows the existing convention: columnDatabaseKey upper-cases its arguments
// and DBCache.Column matches with strings.EqualFold.
func catalogCacheKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// HasCatalog reports whether the active repository produced an extended
// catalog. It is the single gate a feature uses before touching catalog data.
func (dc *DBCache) HasCatalog() bool {
	return dc != nil && dc.Catalog != nil
}

func (dc *DBCache) View(name string) (*ViewDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	view, ok := dc.Catalog.Views[catalogCacheKey(name)]
	return view, ok
}

func (dc *DBCache) Procedure(name string) (*ProcedureDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	procedure, ok := dc.Catalog.Procedures[catalogCacheKey(name)]
	return procedure, ok
}

func (dc *DBCache) Generator(name string) (*GeneratorDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	generator, ok := dc.Catalog.Generators[catalogCacheKey(name)]
	return generator, ok
}

func (dc *DBCache) Domain(name string) (*DomainDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	domain, ok := dc.Catalog.Domains[catalogCacheKey(name)]
	return domain, ok
}

func (dc *DBCache) Function(name string) (*FunctionDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	function, ok := dc.Catalog.Functions[catalogCacheKey(name)]
	return function, ok
}

func (dc *DBCache) Index(name string) (*IndexDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	index, ok := dc.Catalog.Indexes[catalogCacheKey(name)]
	return index, ok
}

func (dc *DBCache) Trigger(name string) (*TriggerDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	trigger, ok := dc.Catalog.Triggers[catalogCacheKey(name)]
	return trigger, ok
}

func (dc *DBCache) IndexesForTable(table string) []*IndexDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.IndexesByTable[catalogCacheKey(table)]
}

func (dc *DBCache) TriggersForTable(table string) []*TriggerDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.TriggersByTable[catalogCacheKey(table)]
}

func (dc *DBCache) SortedProcedures() []string {
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

func (dc *DBCache) SortedViews() []string {
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

func (dc *DBCache) SortedGenerators() []string {
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
func (dc *DBCache) SortedFunctions() []string {
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

// GenerateCatalogCache returns nil, false, nil when the repository has no
// extended catalog.
func (u *DBCacheGenerator) GenerateCatalogCache(ctx context.Context) (*CatalogCache, bool, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateCatalogCache(ctx)
}

func (u *DBCacheGenerator) generateCatalogCache(ctx context.Context) (*CatalogCache, bool, error) {
	source, ok := u.repo.(CatalogRepository)
	if !ok {
		return nil, false, nil
	}

	catalog := &CatalogCache{
		Views:           map[string]*ViewDesc{},
		Procedures:      map[string]*ProcedureDesc{},
		Generators:      map[string]*GeneratorDesc{},
		Domains:         map[string]*DomainDesc{},
		Functions:       map[string]*FunctionDesc{},
		Indexes:         map[string]*IndexDesc{},
		IndexesByTable:  map[string][]*IndexDesc{},
		Triggers:        map[string]*TriggerDesc{},
		TriggersByTable: map[string][]*TriggerDesc{},
	}

	views, err := source.DescribeViews(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, view := range views {
		catalog.Views[catalogCacheKey(view.Name)] = view
	}

	procedures, err := source.DescribeProcedures(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, procedure := range procedures {
		catalog.Procedures[catalogCacheKey(procedure.Name)] = procedure
	}

	generators, err := source.DescribeGenerators(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, generator := range generators {
		catalog.Generators[catalogCacheKey(generator.Name)] = generator
	}

	domains, err := source.DescribeDomains(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, domain := range domains {
		catalog.Domains[catalogCacheKey(domain.Name)] = domain
	}

	functions, err := source.DescribeFunctions(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, function := range functions {
		catalog.Functions[catalogCacheKey(function.Name)] = function
	}

	indexes, err := source.DescribeIndexes(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, index := range indexes {
		catalog.Indexes[catalogCacheKey(index.Name)] = index
		if index.RelationName == "" {
			continue
		}
		key := catalogCacheKey(index.RelationName)
		catalog.IndexesByTable[key] = append(catalog.IndexesByTable[key], index)
	}

	triggers, err := source.DescribeTriggers(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, trigger := range triggers {
		catalog.Triggers[catalogCacheKey(trigger.Name)] = trigger
		// A database-level trigger has no relation and is reachable by name
		// only; grouping it under the empty table name would be a phantom.
		if !trigger.RelationName.Valid || strings.TrimSpace(trigger.RelationName.String) == "" {
			continue
		}
		key := catalogCacheKey(trigger.RelationName.String)
		catalog.TriggersByTable[key] = append(catalog.TriggersByTable[key], trigger)
	}

	return catalog, true, nil
}
