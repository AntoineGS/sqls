package duckmeta

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestDuckDBMetadataLiveProviderLoadsAllSchemasAndDetails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "fixture.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatalf("sql.Open(duckdb): %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	seed, err := os.ReadFile("testdata/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(seed)); err != nil {
		t.Fatalf("execute DuckDB metadata seed: %v", err)
	}

	source := countingDuckDBSourceForDB(t, db)
	factory := Factory(Config{Target: path}, func(context.Context) (ConnSource, error) {
		return source, nil
	})
	key, err := KeyFor(Config{Target: path})
	if err != nil {
		t.Fatalf("KeyFor(): %v", err)
	}
	scope, err := ScopeFor(Config{Target: path})
	if err != nil {
		t.Fatalf("ScopeFor(): %v", err)
	}
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	var provider metadata.Provider
	lease, err := registry.Acquire(ctx, key, scope, func(factoryCtx context.Context) (metadata.Provider, error) {
		provider, err = factory(factoryCtx)
		return provider, err
	})
	if err != nil {
		t.Fatalf("Registry.Acquire(): %v", err)
	}
	defer func() {
		lease.Release()
		if err := registry.Close(context.Background()); err != nil {
			t.Errorf("Registry.Close(): %v", err)
		}
	}()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("metadata did not settle: %v", err)
	}
	snapshot := lease.Snapshot()
	if snapshot == nil || snapshot.Cache == nil {
		t.Fatalf("metadata snapshot = %#v, want a populated cache", snapshot)
	}
	assertDuckDBMetadataStates(t, snapshot)
	assertDuckDBSchemaAndColumns(t, snapshot.Cache)
	assertDuckDBKeysIndexesAndObjects(t, snapshot.Cache)
	assertDuckDBOverloadedMacro(t, ctx, db, snapshot.Cache, lease)
	assertDuckDBDetails(t, ctx, db, lease)
	assertDuckDBUnsupportedDetails(t, provider)

	if got := source.calls.Load(); got < int64(len(provider.Plan().Jobs)+1) {
		t.Errorf("ConnSource.Conn calls = %d, want at least one setup call plus each metadata job (%d)", got, len(provider.Plan().Jobs)+1)
	}
	if got := db.Stats().InUse; got != 0 {
		t.Errorf("database connections still in use after metadata jobs: %d", got)
	}
}

func TestDuckDBTypesExposeUserDefinedEnums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "types.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed, err := os.ReadFile("testdata/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT schema_name, type_name, logical_type, internal, labels FROM duckdb_types() WHERE type_name IN ('mood', 'order_state') ORDER BY schema_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]any{
		"main\x00mood":         []any{"calm", "busy"},
		"sales\x00order_state": []any{"open", "closed"},
	}
	seen := make(map[string]bool, len(want))
	for rows.Next() {
		var schema, name, logicalType string
		var internal bool
		var labels any
		if err := rows.Scan(&schema, &name, &logicalType, &internal, &labels); err != nil {
			t.Fatal(err)
		}
		key := schema + "\x00" + name
		seen[key] = true
		if internal || logicalType != "ENUM" || !reflect.DeepEqual(labels, want[key]) {
			t.Errorf("duckdb_types row %q = {logical_type:%q internal:%t labels:%#v}, want a user-defined ENUM with labels %#v", key, logicalType, internal, labels, want[key])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, map[string]bool{"main\x00mood": true, "sales\x00order_state": true}) {
		t.Errorf("DuckDB 2.0 user-defined type rows = %#v, want both seeded ENUMs", seen)
	}
}

func assertDuckDBMetadataStates(t *testing.T, snapshot *catalog.MetadataSnapshot) {
	t.Helper()
	for _, kind := range []catalog.MetadataKind{
		catalog.MetadataSchemas,
		catalog.MetadataRelations,
		catalog.MetadataColumnsCurrent,
		catalog.MetadataPrimaryKeys,
		catalog.MetadataForeignKeys,
		catalog.MetadataViews,
		catalog.MetadataGenerators,
		catalog.MetadataDomains,
		catalog.MetadataFunctions,
		catalog.MetadataIndexes,
	} {
		status, ok := snapshot.Status[kind]
		if !ok || status.State != catalog.MetadataReady {
			t.Errorf("metadata category %s = %+v (present=%t), want ready", kind, status, ok)
		}
	}
	for _, kind := range []catalog.MetadataKind{
		catalog.MetadataColumnsAll,
		catalog.MetadataDependencies,
		catalog.MetadataProcedures,
		catalog.MetadataExceptions,
		catalog.MetadataSystemRelations,
		catalog.MetadataTriggers,
	} {
		status, ok := snapshot.Status[kind]
		if !ok || status.State != catalog.MetadataUnsupported {
			t.Errorf("metadata category %s = %+v (present=%t), want unsupported", kind, status, ok)
		}
	}
}

func assertDuckDBSchemaAndColumns(t *testing.T, cache *catalog.Cache) {
	t.Helper()
	if cache.DefaultSchema != "main" {
		t.Errorf("DefaultSchema = %q, want main", cache.DefaultSchema)
	}
	wantSchemas := map[string]string{"MAIN": "main", "SALES": "sales"}
	if !reflect.DeepEqual(cache.Schemas, wantSchemas) {
		t.Errorf("Schemas = %#v, want %#v", cache.Schemas, wantSchemas)
	}
	for _, excluded := range []string{"INFORMATION_SCHEMA", "PG_CATALOG"} {
		if _, ok := cache.Schemas[excluded]; ok {
			t.Errorf("system schema %q was published: %#v", excluded, cache.Schemas)
		}
	}
	for schemaKey, relation := range map[string]string{"MAIN": "orders", "SALES": "orders"} {
		if !contains(cache.SchemaTables[schemaKey], relation) {
			t.Errorf("SchemaTables[%q] = %v, want relation %q", schemaKey, cache.SchemaTables[schemaKey], relation)
		}
	}
	if !contains(cache.SchemaTables["SALES"], "MixedCase") {
		t.Errorf("sales relation names lost catalog case: %v", cache.SchemaTables["SALES"])
	}

	mainColumns, ok := cache.ColumnDatabase("main", "orders")
	if !ok {
		t.Fatal("main.orders columns are missing")
	}
	salesColumns, ok := cache.ColumnDatabase("sales", "orders")
	if !ok {
		t.Fatal("sales.orders columns are missing")
	}
	if got, want := columnNames(mainColumns), []string{"id", "customer_id", "total"}; !reflect.DeepEqual(got, want) {
		t.Errorf("main.orders columns = %v, want %v", got, want)
	}
	if got, want := columnNames(salesColumns), []string{"order_code", "amount", "region"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sales.orders columns = %v, want %v", got, want)
	}
	for _, column := range append(mainColumns, salesColumns...) {
		if column == nil {
			continue
		}
		if column.Schema == "" || column.Table != "orders" {
			t.Errorf("column has incomplete schema/table identity: %+v", column)
		}
	}
}

func assertDuckDBKeysIndexesAndObjects(t *testing.T, cache *catalog.Cache) {
	t.Helper()
	for _, tc := range []struct {
		schema, table, column, wantKey string
	}{
		{schema: "main", table: "orders", column: "id", wantKey: "PRI"},
		{schema: "main", table: "unique_nonnull", column: "code", wantKey: "UNI"},
	} {
		columns, ok := cache.ColumnDatabase(tc.schema, tc.table)
		if !ok {
			t.Errorf("columns for %s.%s are missing", tc.schema, tc.table)
			continue
		}
		found := false
		for _, column := range columns {
			if column != nil && column.Name == tc.column {
				found = true
				if column.Key != tc.wantKey {
					t.Errorf("%s.%s.%s Key = %q, want %q", tc.schema, tc.table, tc.column, column.Key, tc.wantKey)
				}
			}
		}
		if !found {
			t.Errorf("column %s.%s.%s is missing", tc.schema, tc.table, tc.column)
		}
	}

	if cache.Catalog == nil {
		t.Fatal("extended DuckDB catalog is nil")
	}
	var expressionIndex *catalog.IndexDesc
	for _, index := range cache.Catalog.IndexesByID {
		if index != nil && index.Schema == "main" && index.Name == "idx_lower" {
			expressionIndex = index
			break
		}
	}
	if expressionIndex == nil {
		t.Fatal("expression index main.idx_lower is missing")
	}
	if len(expressionIndex.Columns) != 0 {
		t.Errorf("expression index columns = %v, want no invented column names", expressionIndex.Columns)
	}
	if !hasNamedSchema(cache.Catalog.GeneratorsByID, "main", "seq_id", func(v any) (string, string) {
		desc := v.(*catalog.GeneratorDesc)
		return desc.Schema, desc.Name
	}) {
		t.Error("sequence main.seq_id is missing from generators")
	}
	for _, tc := range []struct{ schema, name string }{{"main", "mood"}, {"sales", "order_state"}} {
		id := catalog.ObjectID{Schema: tc.schema, Kind: catalog.ObjectKindDomain, Name: tc.name}.Key()
		desc := cache.Catalog.DomainsByID[id]
		if desc == nil {
			t.Errorf("user-defined type %s.%s is missing from domains", tc.schema, tc.name)
			continue
		}
		if desc.Schema != tc.schema || desc.Name != tc.name || desc.Type != "ENUM" || desc.Facts == nil || !desc.Facts.UserDomain {
			t.Errorf("DuckDB type descriptor %s.%s = %+v, want exact identity, ENUM and user-defined type fact", tc.schema, tc.name, desc)
		}
	}
	for _, macro := range []string{"add1", "top_orders", "overloaded"} {
		if !hasNamedSchema(cache.Catalog.FunctionsByID, "main", macro, func(v any) (string, string) {
			desc := v.(*catalog.FunctionDesc)
			return desc.Schema, desc.Name
		}) {
			t.Errorf("macro main.%s is missing from functions", macro)
		}
	}

	fkSource := catalog.ColumnDatabaseKey("main", "child_rows")
	fkTarget := catalog.ColumnDatabaseKey("main", "parent_rows")
	keys := cache.ForeignKeys[fkSource][fkTarget]
	if len(keys) != 1 || len(*keys[0]) != 2 {
		t.Fatalf("child_rows → parent_rows foreign keys = %#v, want one two-column key", keys)
	}
	wantPairs := [][2]string{{"parent_tenant_id", "tenant_id"}, {"parent_id", "id"}}
	for i, pair := range *keys[0] {
		if pair[0] == nil || pair[1] == nil {
			t.Fatalf("foreign key pair %d is incomplete: %#v", i, pair)
		}
		if got := [2]string{pair[0].Name, pair[1].Name}; got != wantPairs[i] {
			t.Errorf("foreign key pair %d = %v, want %v", i, got, wantPairs[i])
		}
	}

	dottedSource := catalog.ColumnDatabaseKey("main", "dot_child")
	dottedTarget := catalog.ColumnDatabaseKey("main", "p.dot")
	dottedKeys := cache.ForeignKeys[dottedSource][dottedTarget]
	if len(dottedKeys) != 1 || len(*dottedKeys[0]) != 1 {
		t.Fatalf("dot_child → p.dot foreign keys = %#v, want one one-column key", dottedKeys)
	}
	if pair := (*dottedKeys[0])[0]; pair[0] == nil || pair[1] == nil ||
		pair[1].Schema != "main" || pair[1].Table != "p.dot" || pair[1].Name != "id" {
		t.Errorf("dotted target = %#v, want main.p.dot.id", pair[1])
	}
}

func assertDuckDBDetails(t *testing.T, ctx context.Context, db *sql.DB, lease *metadata.Lease) {
	t.Helper()
	for _, tc := range []struct {
		object catalog.ObjectID
		query  string
		args   []any
	}{
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindTable, Name: "orders"},
			query:  `SELECT sql FROM duckdb_tables() WHERE database_name = current_database() AND schema_name = ? AND table_name = ?`,
			args:   []any{"main", "orders"},
		},
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindView, Name: "v_orders"},
			query:  `SELECT sql FROM duckdb_views() WHERE database_name = current_database() AND schema_name = ? AND view_name = ?`,
			args:   []any{"main", "v_orders"},
		},
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindIndex, Name: "idx_lower"},
			query:  `SELECT sql FROM duckdb_indexes() WHERE database_name = current_database() AND schema_name = ? AND index_name = ?`,
			args:   []any{"main", "idx_lower"},
		},
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindGenerator, Name: "seq_id"},
			query:  `SELECT sql FROM duckdb_sequences() WHERE database_name = current_database() AND schema_name = ? AND sequence_name = ?`,
			args:   []any{"main", "seq_id"},
		},
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindFunction, Name: "add1"},
			query:  `SELECT macro_definition FROM duckdb_functions() WHERE database_name = current_database() AND schema_name = ? AND function_name = ? AND function_type IN ('macro', 'table_macro')`,
			args:   []any{"main", "add1"},
		},
		{
			object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindFunction, Name: "overloaded"},
			query:  `SELECT macro_definition FROM duckdb_functions() WHERE database_name = current_database() AND schema_name = ? AND function_name = ? AND function_type IN ('macro', 'table_macro') ORDER BY macro_definition LIMIT 1`,
			args:   []any{"main", "overloaded"},
		},
	} {
		var want string
		if err := db.QueryRowContext(ctx, tc.query, tc.args...).Scan(&want); err != nil {
			t.Errorf("read expected DDL for %s: %v", tc.object.Name, err)
			continue
		}
		detail, err := lease.Detail(ctx, metadata.Request{Object: tc.object, Facet: "ddl", Consistency: metadata.Committed})
		if err != nil {
			t.Errorf("Detail(%s %s ddl): %v", tc.object.Kind, tc.object.Name, err)
			continue
		}
		if detail.Text != want {
			t.Errorf("Detail(%s %s ddl) = %q, want catalog sql %q", tc.object.Kind, tc.object.Name, detail.Text, want)
		}
	}
}

func assertDuckDBOverloadedMacro(t *testing.T, ctx context.Context, db *sql.DB, cache *catalog.Cache, lease *metadata.Lease) {
	t.Helper()
	var wantDefinition string
	var wantArguments int
	if err := db.QueryRowContext(ctx, `
SELECT macro_definition, array_length(parameters)
FROM duckdb_functions()
WHERE database_name = current_database() AND schema_name = 'main'
  AND function_name = 'overloaded' AND function_type = 'macro'
ORDER BY macro_definition, CAST(parameters AS VARCHAR)
LIMIT 1`).Scan(&wantDefinition, &wantArguments); err != nil {
		t.Fatalf("read canonical overloaded macro: %v", err)
	}
	id := catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindFunction, Name: "overloaded"}.Key()
	desc := cache.Catalog.FunctionsByID[id]
	if desc == nil {
		t.Fatal("overloaded macro is missing from FunctionsByID")
	}
	if got := len(desc.Arguments); got != wantArguments {
		t.Errorf("overloaded macro arguments = %d, want canonical overload's %d", got, wantArguments)
	}
	detail, err := lease.Detail(ctx, metadata.Request{
		Object: catalog.ObjectID{Schema: "main", Kind: catalog.ObjectKindFunction, Name: "overloaded"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("Detail(overloaded): %v", err)
	}
	if detail.Text != wantDefinition {
		t.Errorf("overloaded macro detail = %q, want canonical overload definition %q", detail.Text, wantDefinition)
	}
}

func assertDuckDBUnsupportedDetails(t *testing.T, provider metadata.Provider) {
	t.Helper()
	for _, kind := range []catalog.ObjectKind{
		catalog.ObjectKindTrigger,
		catalog.ObjectKindProcedure,
		catalog.ObjectKindDomain,
		catalog.ObjectKindException,
	} {
		_, err := provider.DetailPlan(metadata.Request{
			Object: catalog.ObjectID{Schema: "main", Kind: kind, Name: "not_used"}, Facet: "ddl",
		})
		if !errors.Is(err, metadata.ErrUnsupported) {
			t.Errorf("DetailPlan(%s) error = %v, want metadata.ErrUnsupported", kind, err)
		}
	}
}

func columnNames(columns []*catalog.ColumnDesc) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		if column != nil {
			names = append(names, column.Name)
		}
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasNamedSchema[T any](values map[string]T, schema, name string, identity func(any) (string, string)) bool {
	for _, value := range values {
		if gotSchema, gotName := identity(value); gotSchema == schema && gotName == name {
			return true
		}
	}
	return false
}
