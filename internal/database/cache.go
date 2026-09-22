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
