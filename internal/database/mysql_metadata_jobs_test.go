package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestMySQLMetadataJobsProjectSchemaScopedCatalog(t *testing.T) {
	state := newMySQLMetadataScript()
	db := sql.OpenDB(mysqlMetadataConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "MDE_FIXTURE"}
	plan := repo.MetadataPlan()

	patches := make(map[catalog.MetadataKind]catalog.MetadataPatch, len(plan.Jobs))
	merged := newMetadataCache()
	for _, job := range plan.Jobs {
		patch, err := job.Run(context.Background(), merged)
		if err != nil {
			t.Fatalf("%s job: %v", job.Kind, err)
		}
		if patch.Cache == nil {
			t.Fatalf("%s job returned nil cache", job.Kind)
		}
		patches[job.Kind] = patch
		merged, err = mergeMetadata(merged, job.Kind, patch)
		if err != nil {
			t.Fatalf("merge %s patch: %v", job.Kind, err)
		}
	}

	assertMySQLSchemasPatch(t, patches[catalog.MetadataSchemas])
	assertMySQLRelationsPatch(t, patches[catalog.MetadataRelations])
	assertMySQLColumnsPatch(t, patches[catalog.MetadataColumnsCurrent])
	assertMySQLViewsPatch(t, patches[catalog.MetadataViews])
	assertMySQLIndexesPatch(t, patches[catalog.MetadataIndexes])
	assertMySQLPrimaryKeysPatch(t, patches[catalog.MetadataPrimaryKeys])
	assertMySQLForeignKeysPatch(t, patches[catalog.MetadataForeignKeys])
	assertMySQLProceduresPatch(t, patches[catalog.MetadataProcedures])
	assertMySQLFunctionsPatch(t, patches[catalog.MetadataFunctions])
	assertMySQLTriggersPatch(t, patches[catalog.MetadataTriggers])
	primaryKeyID := catalog.ObjectID{Schema: merged.DefaultSchema, Kind: catalog.ObjectKindTable, Name: "composite_items"}
	if got, want := merged.PrimaryKeysByID[primaryKeyID.Key()], []string{"tenant_id", "item_id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("merged composite PK = %v, want schema-qualified columns %v", got, want)
	}
	for _, object := range []catalog.ObjectID{
		{Kind: catalog.ObjectKindView, Name: "items_view"},
		catalog.IndexObjectID("mde_fixture", "items", "uniq_email"),
		{Kind: catalog.ObjectKindProcedure, Name: "save_items"},
		{Kind: catalog.ObjectKindFunction, Name: "item_count"},
		{Kind: catalog.ObjectKindTrigger, Name: "items_bi"},
	} {
		if merged.Catalog == nil || merged.Catalog.Objects == nil {
			t.Fatal("merged catalog object index is nil")
		}
		if got, ok := merged.Catalog.Objects.Lookup(object); !ok || got.Name != object.Name {
			t.Errorf("merged catalog omitted object %+v (got %+v, found=%v)", object, got, ok)
		}
	}
	compositeIndexes := merged.Catalog.IndexesByTable[catalog.CatalogCacheKey("composite_items")]
	if len(compositeIndexes) != 1 || compositeIndexes[0].Name != "PRIMARY" || compositeIndexes[0].RelationName != "composite_items" {
		t.Fatalf("merged composite-table indexes = %#v, want its table-local PRIMARY index", compositeIndexes)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.options) != len(plan.Jobs) {
		t.Fatalf("transactions = %d, want one per job (%d)", len(state.options), len(plan.Jobs))
	}
	for i, options := range state.options {
		if options.ReadOnly != true || options.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
			t.Errorf("BeginTx[%d] options = %+v, want read-only read-committed", i, options)
		}
	}
	if state.commits != len(plan.Jobs) || state.rollbacks != 0 {
		t.Fatalf("transactions committed=%d rolled back=%d, want %d commits and no rollbacks", state.commits, state.rollbacks, len(plan.Jobs))
	}
	if len(state.queries) == 0 {
		t.Fatal("metadata jobs issued no queries")
	}
	for _, query := range state.queries {
		if !mysqlMetadataHasSchemaPredicate(query) {
			t.Errorf("metadata query is not schema-scoped: %q", query)
		}
		wantSchema := "mde_fixture"
		if strings.Contains(strings.ToUpper(query), "INFORMATION_SCHEMA.SCHEMATA") {
			wantSchema = "MDE_FIXTURE"
		}
		bound := false
		for _, argument := range state.args {
			if strings.Contains(query, "?") && argument.Query == query && argument.Value == wantSchema {
				bound = true
				break
			}
		}
		if !bound {
			t.Errorf("schema was not bound for query %q; args = %+v", query, state.args)
		}
	}
	for _, kind := range []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataColumnsCurrent} {
		found := false
		for _, query := range state.queries {
			if strings.Contains(query, "information_schema.TABLES") && kind == catalog.MetadataRelations ||
				strings.Contains(query, "information_schema.COLUMNS") && kind == catalog.MetadataColumnsCurrent {
				found = found || strings.Contains(query, "TABLE_SCHEMA = ?")
			}
		}
		if !found {
			t.Errorf("%s query does not bind TABLE_SCHEMA = ?", kind)
		}
	}
}

func TestMySQLMetadataJobsRollbackAndDiscardFailedPatch(t *testing.T) {
	wantErr := errors.New("scripted query error")
	state := newMySQLMetadataScript()
	db := sql.OpenDB(mysqlMetadataConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "mde_fixture"}
	plan := repo.MetadataPlan()
	cache := mysqlMetadataSchemaCache(t, repo)
	state.mu.Lock()
	state.queryErr = wantErr
	state.mu.Unlock()
	patch, err := plan.Jobs[1].Run(context.Background(), cache)
	if !errors.Is(err, wantErr) {
		t.Fatalf("job error = %v, want %v", err, wantErr)
	}
	if patch.Cache != nil {
		t.Fatalf("failed job cache = %#v, want nil", patch.Cache)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.rollbacks != 1 || state.commits != 1 {
		t.Fatalf("transactions committed=%d rolled back=%d, want schema commit plus failed-job rollback", state.commits, state.rollbacks)
	}
}

func TestMySQLMetadataJobsCancelInFlightQueryAndDiscardPatch(t *testing.T) {
	state := newMySQLMetadataScript()
	db := sql.OpenDB(mysqlMetadataConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "mde_fixture"}
	plan := repo.MetadataPlan()
	cache := mysqlMetadataSchemaCache(t, repo)
	state.mu.Lock()
	state.waitForCancel = true
	state.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		patch catalog.MetadataPatch
		err   error
	}
	done := make(chan result, 1)
	go func() {
		patch, err := plan.Jobs[1].Run(ctx, cache)
		done <- result{patch: patch, err: err}
	}()
	<-state.queryStarted
	cancel()
	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("job error = %v, want context.Canceled", got.err)
	}
	if got.patch.Cache != nil {
		t.Fatalf("cancelled job cache = %#v, want nil", got.patch.Cache)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.rollbacks != 1 || state.commits != 1 {
		t.Fatalf("transactions committed=%d rolled back=%d, want schema commit plus cancelled-job rollback", state.commits, state.rollbacks)
	}
}

func TestMySQLMetadataPlanHasOnlySupportedBoundedJobs(t *testing.T) {
	plan := (&MySQLMetadataRepository{}).MetadataPlan()
	want := []catalog.MetadataKind{
		catalog.MetadataSchemas, catalog.MetadataRelations, catalog.MetadataColumnsCurrent,
		catalog.MetadataViews, catalog.MetadataIndexes, catalog.MetadataPrimaryKeys,
		catalog.MetadataForeignKeys, catalog.MetadataProcedures, catalog.MetadataFunctions,
		catalog.MetadataTriggers,
	}
	if plan.Parallelism != 3 {
		t.Fatalf("parallelism = %d, want 3", plan.Parallelism)
	}
	if len(plan.Jobs) != len(want) {
		t.Fatalf("job count = %d, want %d", len(plan.Jobs), len(want))
	}
	got := make([]catalog.MetadataKind, 0, len(plan.Jobs))
	for _, job := range plan.Jobs {
		got = append(got, job.Kind)
		if job.Run == nil {
			t.Errorf("%s job has nil Run", job.Kind)
		}
		if job.Kind == catalog.MetadataSchemas {
			if len(job.DependsOn) != 0 {
				t.Errorf("schema job dependencies = %v, want none", job.DependsOn)
			}
		} else if !containsMySQLMetadataKind(job.DependsOn, catalog.MetadataSchemas) {
			t.Errorf("%s job dependencies = %v, want schema discovery", job.Kind, job.DependsOn)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("job kinds = %v, want %v", got, want)
	}
	for _, unsupported := range []catalog.MetadataKind{
		catalog.MetadataColumnsAll, catalog.MetadataDependencies, catalog.MetadataGenerators,
		catalog.MetadataDomains, catalog.MetadataExceptions, catalog.MetadataSystemRelations,
	} {
		for _, job := range plan.Jobs {
			if job.Kind == unsupported {
				t.Errorf("unsupported MySQL category %s was scheduled", unsupported)
			}
		}
	}
}

func containsMySQLMetadataKind(kinds []catalog.MetadataKind, want catalog.MetadataKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func TestMySQLMetadataIndexesRetainSameNamedIndexesByTable(t *testing.T) {
	state := newMySQLMetadataScript()
	db := sql.OpenDB(mysqlMetadataConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "mde_fixture"}
	plan := repo.MetadataPlan()
	var schemaJob, indexJob catalog.MetadataJob
	for _, job := range plan.Jobs {
		switch job.Kind {
		case catalog.MetadataSchemas:
			schemaJob = job
		case catalog.MetadataIndexes:
			indexJob = job
		}
	}
	schemaPatch, err := schemaJob.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := mergeMetadata(newMetadataCache(), catalog.MetadataSchemas, schemaPatch)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := indexJob.Run(context.Background(), cache)
	if err != nil {
		t.Fatal(err)
	}
	indexesByID := patch.Cache.Catalog.IndexesByID
	if len(indexesByID) != 3 {
		t.Fatalf("indexes by identity = %#v, want 3 fixture indexes including both table PRIMARY indexes", indexesByID)
	}
	primaryTables := make(map[string]bool)
	for _, index := range indexesByID {
		if index.Name == "PRIMARY" {
			primaryTables[index.RelationName] = true
		}
	}
	if !primaryTables["items"] || !primaryTables["composite_items"] || len(primaryTables) != 2 {
		t.Fatalf("PRIMARY indexes by table = %#v, want items and composite_items", primaryTables)
	}
	for table := range primaryTables {
		if index, ok := patch.Cache.IndexByTableAndID("mde_fixture", table, "PRIMARY"); !ok || index.RelationName != table {
			t.Errorf("IndexByTableAndID(%q, %q, PRIMARY) = %#v, %v", "mde_fixture", table, index, ok)
		}
	}
	if _, ok := patch.Cache.IndexByID("PRIMARY"); ok {
		t.Fatal("name-only PRIMARY lookup returned an arbitrary table's index")
	}
	if index, ok := patch.Cache.IndexByID("uniq_email"); !ok || index.RelationName != "items" {
		t.Fatalf("unique name-only index lookup = %#v, %v", index, ok)
	}
	merged, err := mergeMetadata(cache, catalog.MetadataIndexes, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Catalog.Objects.LookupUnquoted(catalog.ObjectKindIndex, "PRIMARY"); len(got) != 2 {
		t.Fatalf("merged PRIMARY index identities = %#v, want one per table", got)
	}
	for table := range primaryTables {
		want := catalog.IndexObjectID("mde_fixture", table, "PRIMARY")
		if got, ok := merged.Catalog.Objects.Lookup(want); !ok || got != want {
			t.Errorf("merged exact index identity = %+v, %v; want %+v", got, ok, want)
		}
	}
}

func assertMySQLSchemasPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	if patch.Cache.DefaultSchema != "mde_fixture" || !reflect.DeepEqual(patch.Cache.Schemas, map[string]string{"MDE_FIXTURE": "mde_fixture"}) {
		t.Fatalf("schema projection = default %q schemas %#v", patch.Cache.DefaultSchema, patch.Cache.Schemas)
	}
}

func assertMySQLRelationsPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	want := []string{"composite_items", "items", "items_view"}
	if !reflect.DeepEqual(patch.Cache.SchemaTables["MDE_FIXTURE"], want) {
		t.Fatalf("relations = %#v, want %v", patch.Cache.SchemaTables, want)
	}
	if _, ok := patch.Cache.SchemaTables["MDE_OTHER"]; ok {
		t.Fatal("out-of-scope relation was published")
	}
}

func assertMySQLColumnsPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	itemsKey := catalog.ColumnDatabaseKey("mde_fixture", "items")
	columns := patch.Cache.ColumnsWithParent[itemsKey]
	wantNames := []string{"id", "email", "group_id", "payload"}
	if len(columns) != len(wantNames) {
		t.Fatalf("items columns = %#v, want %v", columns, wantNames)
	}
	for i, column := range columns {
		if column.Schema != "mde_fixture" || column.Table != "items" || column.Name != wantNames[i] {
			t.Errorf("items column[%d] = %#v", i, column)
		}
	}
	id := columns[0]
	if id.Type != "bigint unsigned" || id.Null != "NO" || id.Key != "PRI" || id.Default.Valid || id.Extra != "auto_increment" || id.Facts != nil {
		t.Fatalf("MySQL column descriptor = %#v, want exact COLUMN_TYPE/NULL/KEY/NULL DEFAULT/EXTRA and nil Facts", id)
	}
	if len(patch.Cache.ColumnsByID[catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "items"}.Key()]) != 4 {
		t.Fatal("exact-identity columns were not published")
	}
	if _, ok := patch.Cache.ColumnsWithParent[catalog.ColumnDatabaseKey("mde_other", "items")]; ok {
		t.Fatal("out-of-scope column descriptor was published")
	}
}

func assertMySQLViewsPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	view, ok := patch.Cache.Catalog.Views[catalog.CatalogCacheKey("items_view")]
	if !ok || view.Schema != "mde_fixture" || view.Name != "items_view" || view.ViewSource.String != "SELECT id FROM items" || len(view.Columns) != 1 || view.Columns[0].Name != "view_id" {
		t.Fatalf("view projection = %#v, %v", view, ok)
	}
	if _, ok := patch.Cache.Catalog.ViewsByID[catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "items_view"}.Key()]; !ok {
		t.Fatal("exact-identity view was not published")
	}
	if got := patch.Cache.Catalog.Views[catalog.CatalogCacheKey("items_view")]; got == nil || got.Schema != "mde_fixture" {
		t.Fatal("same-named out-of-scope view replaced the fixture view")
	}
}

func assertMySQLIndexesPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	index, ok := patch.Cache.Catalog.Indexes[catalog.CatalogCacheKey("uniq_email")]
	if !ok || index.Schema != "mde_fixture" || index.RelationName != "items" || !index.Unique.Valid || !index.Unique.Bool || !reflect.DeepEqual(index.Columns, []string{"email"}) {
		t.Fatalf("unique index = %#v, %v", index, ok)
	}
	if len(patch.Cache.Catalog.IndexesByTable[catalog.CatalogCacheKey("items")]) != 2 {
		t.Fatalf("indexes by table = %#v, want primary and unique indexes", patch.Cache.Catalog.IndexesByTable)
	}
	if _, ok := patch.Cache.Catalog.IndexesByID[catalog.IndexObjectID("mde_fixture", "items", "uniq_email").Key()]; !ok {
		t.Fatal("exact-identity index was not published")
	}
	if got := patch.Cache.Catalog.Indexes[catalog.CatalogCacheKey("uniq_email")]; got == nil || got.Schema != "mde_fixture" {
		t.Fatal("same-named out-of-scope index replaced the fixture index")
	}
}

func assertMySQLPrimaryKeysPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	relationKey := catalog.ColumnDatabaseKey("mde_fixture", "composite_items")
	want := []string{"tenant_id", "item_id"}
	id := catalog.ObjectID{Schema: "mde_fixture", Kind: catalog.ObjectKindTable, Name: "composite_items"}
	if got := patch.Cache.PrimaryKeysByID[id.Key()]; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered composite PK = %v, want %v", got, want)
	}
	if got := patch.Cache.PrimaryKeysByID[catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "composite_items"}.Key()]; got != nil {
		t.Fatalf("schema-less composite PK identity unexpectedly published: %v", got)
	}
	keys := patch.Cache.PrimaryKeyColumns[relationKey]
	if len(keys) != 2 {
		t.Fatalf("composite PK lookup = %#v, want two columns", keys)
	}
	if _, ok := patch.Cache.PrimaryKeyColumns[catalog.ColumnDatabaseKey("mde_other", "composite_items")]; ok {
		t.Fatal("out-of-scope primary key was published")
	}
}

func assertMySQLForeignKeysPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	source := catalog.ColumnDatabaseKey("mde_fixture", "composite_items")
	target := catalog.ColumnDatabaseKey("mde_fixture", "parents")
	keys := patch.Cache.ForeignKeys[source][target]
	if len(keys) != 1 || len(*keys[0]) != 2 {
		t.Fatalf("composite FK mapping = %#v", keys)
	}
	want := [][2]catalog.ColumnBase{
		{{Schema: "mde_fixture", Table: "composite_items", Name: "tenant_id"}, {Schema: "mde_fixture", Table: "parents", Name: "tenant_id"}},
		{{Schema: "mde_fixture", Table: "composite_items", Name: "item_id"}, {Schema: "mde_fixture", Table: "parents", Name: "item_id"}},
	}
	for i, pair := range *keys[0] {
		if *pair[0] != want[i][0] || *pair[1] != want[i][1] {
			t.Errorf("FK pair[%d] = %#v, want %#v", i, pair, want[i])
		}
	}
	if _, ok := patch.Cache.ForeignKeys[catalog.ColumnDatabaseKey("mde_other", "composite_items")]; ok {
		t.Fatal("out-of-scope foreign key was published")
	}
}

func assertMySQLProceduresPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	procedure, ok := patch.Cache.Catalog.Procedures[catalog.CatalogCacheKey("save_items")]
	if !ok || procedure.Schema != "mde_fixture" || procedure.Source.String != "BEGIN SELECT 1; END" || len(procedure.InputParameters) != 2 || len(procedure.OutputParameters) != 1 {
		t.Fatalf("procedure projection = %#v, %v", procedure, ok)
	}
	if got := []string{procedure.InputParameters[0].Name, procedure.InputParameters[1].Name}; !reflect.DeepEqual(got, []string{"item_id", "email"}) {
		t.Errorf("ordered input parameters = %v", got)
	}
	if got := procedure.OutputParameters[0]; got.Name != "saved_id" || got.Type != "bigint" || got.Nullable.Valid {
		t.Errorf("output parameter = %#v", got)
	}
	if _, ok := patch.Cache.Catalog.ProceduresByID[catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "save_items"}.Key()]; !ok {
		t.Fatal("exact-identity procedure was not published")
	}
	if got := patch.Cache.Catalog.Procedures[catalog.CatalogCacheKey("save_items")]; got == nil || got.Schema != "mde_fixture" {
		t.Fatal("same-named out-of-scope procedure replaced the fixture procedure")
	}
}

func assertMySQLFunctionsPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	function, ok := patch.Cache.Catalog.Functions[catalog.CatalogCacheKey("item_count")]
	if !ok || function.Schema != "mde_fixture" || function.ReturnType != "bigint unsigned" || len(function.Arguments) != 2 {
		t.Fatalf("function projection = %#v, %v", function, ok)
	}
	if got := []string{function.Arguments[0].Name, function.Arguments[1].Name}; !reflect.DeepEqual(got, []string{"tenant_id", "item_id"}) {
		t.Errorf("ordered function arguments = %v", got)
	}
	if _, ok := patch.Cache.Catalog.FunctionsByID[catalog.ObjectID{Kind: catalog.ObjectKindFunction, Name: "item_count"}.Key()]; !ok {
		t.Fatal("exact-identity function was not published")
	}
	if got := patch.Cache.Catalog.Functions[catalog.CatalogCacheKey("item_count")]; got == nil || got.Schema != "mde_fixture" {
		t.Fatal("same-named out-of-scope function replaced the fixture function")
	}
}

func assertMySQLTriggersPatch(t *testing.T, patch catalog.MetadataPatch) {
	t.Helper()
	trigger, ok := patch.Cache.Catalog.Triggers[catalog.CatalogCacheKey("items_bi")]
	if !ok || trigger.Schema != "mde_fixture" || !trigger.RelationName.Valid || trigger.RelationName.String != "items" || trigger.Event != "BEFORE INSERT" || trigger.Source.String != "SET NEW.email = LOWER(NEW.email)" {
		t.Fatalf("trigger projection = %#v, %v", trigger, ok)
	}
	if _, ok := patch.Cache.Catalog.TriggersByID[catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: "items_bi"}.Key()]; !ok {
		t.Fatal("exact-identity trigger was not published")
	}
	if len(patch.Cache.Catalog.TriggersByTable[catalog.CatalogCacheKey("items")]) != 1 {
		t.Fatal("trigger was not grouped by relation")
	}
	if got := patch.Cache.Catalog.Triggers[catalog.CatalogCacheKey("items_bi")]; got == nil || got.Schema != "mde_fixture" {
		t.Fatal("same-named out-of-scope trigger replaced the fixture trigger")
	}
}

func mysqlMetadataHasSchemaPredicate(query string) bool {
	upper := strings.ToUpper(query)
	for _, column := range []string{"SCHEMA_NAME", "TABLE_SCHEMA", "CONSTRAINT_SCHEMA", "ROUTINE_SCHEMA", "SPECIFIC_SCHEMA", "TRIGGER_SCHEMA"} {
		if strings.Contains(upper, column+" = ?") {
			return true
		}
	}
	return false
}

type mysqlMetadataScript struct {
	mu             sync.Mutex
	options        []driver.TxOptions
	queries        []string
	args           []mysqlMetadataArgument
	commits        int
	rollbacks      int
	queryErr       error
	waitForCancel  bool
	queryStarted   chan struct{}
	queryStartedMu sync.Once
}

type mysqlMetadataArgument struct {
	Query string
	Value any
}

func newMySQLMetadataScript() *mysqlMetadataScript {
	return &mysqlMetadataScript{queryStarted: make(chan struct{})}
}

func mysqlMetadataSchemaCache(t *testing.T, repo *MySQLMetadataRepository) *catalog.Cache {
	t.Helper()
	var schemaJob *catalog.MetadataJob
	for _, job := range repo.MetadataPlan().Jobs {
		if job.Kind == catalog.MetadataSchemas {
			copy := job
			schemaJob = &copy
			break
		}
	}
	if schemaJob == nil {
		t.Fatal("metadata plan has no schema job")
	}
	patch, err := schemaJob.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("schema job: %v", err)
	}
	cache, err := mergeMetadata(newMetadataCache(), catalog.MetadataSchemas, patch)
	if err != nil {
		t.Fatalf("merge schema patch: %v", err)
	}
	return cache
}

type mysqlMetadataConnector struct{ state *mysqlMetadataScript }

func (c mysqlMetadataConnector) Connect(context.Context) (driver.Conn, error) {
	return mysqlMetadataConn{state: c.state}, nil
}

func (c mysqlMetadataConnector) Driver() driver.Driver { return mysqlMetadataDriver{state: c.state} }

type mysqlMetadataDriver struct{ state *mysqlMetadataScript }

func (d mysqlMetadataDriver) Open(string) (driver.Conn, error) {
	return mysqlMetadataConn{state: d.state}, nil
}

type mysqlMetadataConn struct{ state *mysqlMetadataScript }

func (mysqlMetadataConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (mysqlMetadataConn) Close() error                        { return nil }
func (c mysqlMetadataConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c mysqlMetadataConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	c.state.options = append(c.state.options, options)
	c.state.mu.Unlock()
	return mysqlMetadataTx{state: c.state}, nil
}

func (c mysqlMetadataConn) QueryContext(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	for _, argument := range arguments {
		c.state.args = append(c.state.args, mysqlMetadataArgument{Query: query, Value: argument.Value})
	}
	waitForCancel, queryErr := c.state.waitForCancel, c.state.queryErr
	c.state.mu.Unlock()
	if waitForCancel {
		c.state.queryStartedMu.Do(func() { close(c.state.queryStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if queryErr != nil {
		return nil, queryErr
	}
	return mysqlMetadataRowsFor(query), nil
}

type mysqlMetadataTx struct{ state *mysqlMetadataScript }

func (tx mysqlMetadataTx) Commit() error {
	tx.state.mu.Lock()
	tx.state.commits++
	tx.state.mu.Unlock()
	return nil
}

func (tx mysqlMetadataTx) Rollback() error {
	tx.state.mu.Lock()
	tx.state.rollbacks++
	tx.state.mu.Unlock()
	return nil
}

type mysqlMetadataScriptRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *mysqlMetadataScriptRows) Columns() []string { return r.columns }
func (*mysqlMetadataScriptRows) Close() error        { return nil }
func (r *mysqlMetadataScriptRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}

func mysqlMetadataRowsFor(query string) driver.Rows {
	upper := strings.ToUpper(query)
	var columns []string
	var rows [][]driver.Value
	switch {
	case strings.Contains(upper, "INFORMATION_SCHEMA.SCHEMATA"):
		columns = []string{"SCHEMA_NAME"}
		rows = [][]driver.Value{{"mde_fixture"}}
	case strings.Contains(upper, "INFORMATION_SCHEMA.TABLES"):
		columns = []string{"TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE"}
		rows = [][]driver.Value{{"mde_fixture", "composite_items", "BASE TABLE"}, {"mde_fixture", "items", "BASE TABLE"}, {"mde_fixture", "items_view", "VIEW"}, {"mde_other", "items", "BASE TABLE"}}
	case strings.Contains(upper, "INFORMATION_SCHEMA.COLUMNS"):
		columns = []string{"TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "COLUMN_KEY", "COLUMN_DEFAULT", "EXTRA", "ORDINAL_POSITION"}
		rows = [][]driver.Value{
			{"mde_fixture", "items", "id", "bigint unsigned", "NO", "PRI", nil, "auto_increment", int64(1)},
			{"mde_fixture", "items", "email", "varchar(120)", "NO", "UNI", nil, "", int64(2)},
			{"mde_fixture", "items", "group_id", "bigint", "YES", "MUL", nil, "", int64(3)},
			{"mde_fixture", "items", "payload", "json", "YES", "", nil, "", int64(4)},
			{"mde_fixture", "items_view", "view_id", "bigint unsigned", "NO", "", nil, "", int64(1)},
			{"mde_fixture", "composite_items", "tenant_id", "bigint", "NO", "PRI", nil, "", int64(1)},
			{"mde_fixture", "composite_items", "item_id", "bigint", "NO", "PRI", nil, "", int64(2)},
			{"mde_other", "items", "id", "int", "NO", "PRI", nil, "", int64(1)},
		}
	case strings.Contains(upper, "INFORMATION_SCHEMA.VIEWS"):
		columns = []string{"TABLE_SCHEMA", "TABLE_NAME", "VIEW_DEFINITION", "DEFINER"}
		rows = [][]driver.Value{{"mde_fixture", "items_view", "SELECT id FROM items", "fixture@localhost"}, {"mde_other", "items_view", "SELECT 1", "other@localhost"}}
	case strings.Contains(upper, "INFORMATION_SCHEMA.STATISTICS"):
		columns = []string{"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "NON_UNIQUE", "SEQ_IN_INDEX", "COLUMN_NAME"}
		rows = [][]driver.Value{
			{"mde_fixture", "composite_items", "PRIMARY", int64(0), int64(2), "item_id"},
			{"mde_fixture", "items", "PRIMARY", int64(0), int64(1), "id"},
			{"mde_fixture", "items", "uniq_email", int64(0), int64(1), "email"},
			{"mde_other", "items", "uniq_email", int64(0), int64(1), "id"},
			{"mde_fixture", "composite_items", "PRIMARY", int64(0), int64(1), "tenant_id"},
		}
	case strings.Contains(upper, "INFORMATION_SCHEMA.TABLE_CONSTRAINTS"):
		columns = []string{"TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "ORDINAL_POSITION"}
		rows = [][]driver.Value{{"mde_fixture", "composite_items", "item_id", int64(2)}, {"mde_fixture", "composite_items", "tenant_id", int64(1)}, {"mde_other", "composite_items", "tenant_id", int64(1)}}
	case strings.Contains(upper, "INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS"):
		columns = []string{"CONSTRAINT_SCHEMA", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME", "REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME", "CONSTRAINT_NAME", "ORDINAL_POSITION"}
		rows = [][]driver.Value{
			{"mde_fixture", "mde_fixture", "composite_items", "item_id", "mde_fixture", "parents", "item_id", "fk_composite", int64(2)},
			{"mde_fixture", "mde_fixture", "composite_items", "tenant_id", "mde_fixture", "parents", "tenant_id", "fk_composite", int64(1)},
			{"mde_other", "mde_other", "composite_items", "tenant_id", "mde_other", "parents", "tenant_id", "fk_composite", int64(1)},
		}
	case strings.Contains(upper, "INFORMATION_SCHEMA.PARAMETERS"):
		columns = []string{"SPECIFIC_SCHEMA", "SPECIFIC_NAME", "PARAMETER_NAME", "PARAMETER_MODE", "DTD_IDENTIFIER", "ORDINAL_POSITION"}
		if strings.Contains(upper, "ROUTINE_TYPE = 'PROCEDURE'") {
			rows = [][]driver.Value{
				{"mde_fixture", "save_items", "email", "IN", "varchar(120)", int64(2)},
				{"mde_fixture", "save_items", "saved_id", "OUT", "bigint", int64(3)},
				{"mde_fixture", "save_items", "item_id", "IN", "bigint unsigned", int64(1)},
				{"mde_other", "save_items", "other_id", "IN", "int", int64(1)},
			}
		} else {
			rows = [][]driver.Value{
				{"mde_fixture", "item_count", nil, nil, "bigint unsigned", int64(0)},
				{"mde_fixture", "item_count", "item_id", "IN", "bigint unsigned", int64(2)},
				{"mde_fixture", "item_count", "tenant_id", "IN", "bigint", int64(1)},
				{"mde_other", "item_count", nil, nil, "int", int64(0)},
			}
		}
	case strings.Contains(upper, "INFORMATION_SCHEMA.ROUTINES"):
		columns = []string{"ROUTINE_SCHEMA", "ROUTINE_NAME", "ROUTINE_TYPE", "ROUTINE_DEFINITION", "DEFINER", "ROUTINE_COMMENT"}
		if strings.Contains(upper, "ROUTINE_TYPE = 'PROCEDURE'") {
			rows = [][]driver.Value{{"mde_fixture", "save_items", "PROCEDURE", "BEGIN SELECT 1; END", "fixture@localhost", "save rows"}, {"mde_other", "save_items", "PROCEDURE", "SELECT 1", "other@localhost", ""}}
		} else {
			rows = [][]driver.Value{{"mde_fixture", "item_count", "FUNCTION", "RETURN 1", "fixture@localhost", "count rows"}, {"mde_other", "item_count", "FUNCTION", "RETURN 1", "other@localhost", ""}}
		}
	case strings.Contains(upper, "INFORMATION_SCHEMA.TRIGGERS"):
		columns = []string{"TRIGGER_SCHEMA", "TRIGGER_NAME", "EVENT_OBJECT_SCHEMA", "EVENT_OBJECT_TABLE", "ACTION_TIMING", "EVENT_MANIPULATION", "ACTION_STATEMENT"}
		rows = [][]driver.Value{{"mde_fixture", "items_bi", "mde_fixture", "items", "BEFORE", "INSERT", "SET NEW.email = LOWER(NEW.email)"}, {"mde_other", "items_bi", "mde_other", "items", "AFTER", "INSERT", "SELECT 1"}}
	default:
		panic("unexpected MySQL metadata query: " + query)
	}
	return &mysqlMetadataScriptRows{columns: columns, rows: rows}
}
