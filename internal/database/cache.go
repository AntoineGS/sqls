package database

import (
	"context"
	"log"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

// columnDatabaseKey, catalogCacheKey and genColumnMap are thin delegates to
// the catalog package's exported equivalents, kept under their original
// unexported names so every existing call site in this package needs no edit.
func columnDatabaseKey(dbName, tableName string) string {
	return catalog.ColumnDatabaseKey(dbName, tableName)
}
func catalogCacheKey(name string) string { return catalog.CatalogCacheKey(name) }
func genColumnMap(columnDescs []*ColumnDesc) map[string][]*ColumnDesc {
	return catalog.GenColumnMap(columnDescs)
}

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
	dbCache.DefaultSchema, err = u.repo.CurrentSchema(ctx)
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

	if dbCache.DefaultSchema == "" {
		var topKey string
		for k := range dbCache.Schemas {
			topKey = k
			continue
		}
		dbCache.DefaultSchema = dbCache.Schemas[topKey]
	}
	schemaTables, err := u.repo.SchemaTables(ctx)
	if err != nil {
		return nil, err
	}
	dbCache.SchemaTables = make(map[string][]string)
	for index, element := range schemaTables {
		dbCache.SchemaTables[strings.ToUpper(index)] = element
	}

	dbCache.ColumnsWithParent, err = u.genColumnCacheCurrent(ctx, dbCache.DefaultSchema)
	if err != nil {
		return nil, err
	}
	dbCache.ForeignKeys, err = u.genForeignKeysCache(ctx, dbCache.DefaultSchema)
	if err != nil {
		return nil, err
	}
	dbCache.Metadata = map[MetadataKind]MetadataState{
		MetadataSchemas:        MetadataReady,
		MetadataRelations:      MetadataReady,
		MetadataColumnsCurrent: MetadataReady,
		MetadataForeignKeys:    MetadataReady,
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

	catalogCache := &CatalogCache{
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
		catalogCache.Views[catalogCacheKey(view.Name)] = view
	}

	procedures, err := source.DescribeProcedures(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, procedure := range procedures {
		catalogCache.Procedures[catalogCacheKey(procedure.Name)] = procedure
	}

	generators, err := source.DescribeGenerators(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, generator := range generators {
		catalogCache.Generators[catalogCacheKey(generator.Name)] = generator
	}

	domains, err := source.DescribeDomains(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, domain := range domains {
		catalogCache.Domains[catalogCacheKey(domain.Name)] = domain
	}

	functions, err := source.DescribeFunctions(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, function := range functions {
		catalogCache.Functions[catalogCacheKey(function.Name)] = function
	}

	indexes, err := source.DescribeIndexes(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, index := range indexes {
		catalogCache.Indexes[catalogCacheKey(index.Name)] = index
		if index.RelationName == "" {
			continue
		}
		key := catalogCacheKey(index.RelationName)
		catalogCache.IndexesByTable[key] = append(catalogCache.IndexesByTable[key], index)
	}

	triggers, err := source.DescribeTriggers(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, trigger := range triggers {
		catalogCache.Triggers[catalogCacheKey(trigger.Name)] = trigger
		// A database-level trigger has no relation and is reachable by name
		// only; grouping it under the empty table name would be a phantom.
		if !trigger.RelationName.Valid || strings.TrimSpace(trigger.RelationName.String) == "" {
			continue
		}
		key := catalogCacheKey(trigger.RelationName.String)
		catalogCache.TriggersByTable[key] = append(catalogCache.TriggersByTable[key], trigger)
	}

	return catalogCache, true, nil
}
