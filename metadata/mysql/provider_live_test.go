package mymeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestLiveMySQLMetadataProviderLoadsSchemaScopedCatalogAndDetails(t *testing.T) {
	cfg := mysql8LiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	key, err := KeyFor(cfg)
	if err != nil {
		t.Fatalf("KeyFor() error = %v", err)
	}
	scope, err := ScopeFor(cfg)
	if err != nil {
		t.Fatalf("ScopeFor() error = %v", err)
	}
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Errorf("Registry.Close() error = %v", err)
		}
	}()

	var liveProvider *mysqlProvider
	factory := Factory(cfg)
	lease, err := registry.Acquire(ctx, key, scope, func(factoryCtx context.Context) (metadata.Provider, error) {
		provider, err := factory(factoryCtx)
		if err == nil {
			var ok bool
			liveProvider, ok = provider.(*mysqlProvider)
			if !ok {
				return nil, fmt.Errorf("MySQL factory returned %T, want *mysqlProvider", provider)
			}
		}
		return provider, err
	})
	if err != nil {
		t.Fatalf("Registry.Acquire() error = %v", err)
	}
	defer lease.Release()

	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("metadata did not settle: %v", err)
	}
	snapshot := lease.Snapshot()
	if snapshot == nil || snapshot.StartFailed {
		t.Fatalf("metadata snapshot = %#v, want a started snapshot", snapshot)
	}
	if snapshot.Cache == nil {
		t.Fatal("metadata snapshot has no cache")
	}
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		t.Fatalf("ParseTarget() error = %v", err)
	}
	assertMySQLMetadataStates(t, snapshot)
	assertMySQLSchemaScope(t, snapshot.Cache, target.Schema)
	assertMySQLNativeColumnKeys(t, ctx, snapshot.Cache, liveProvider.conn)
	assertMySQLWrongSchemaDetailsDoNotQuery(t, ctx, liveProvider, lease)
	assertMySQLCreateDetails(t, ctx, liveProvider, lease, target.Schema)
}

func mysql8LiveConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("MYSQL8_TEST_DSN") == "" {
		t.Skip("MYSQL8_TEST_DSN is not set")
	}
	addr := os.Getenv("MYSQL8_TEST_ADDR")
	user := os.Getenv("MYSQL8_TEST_USER")
	password := os.Getenv("MYSQL8_TEST_PASSWORD")
	schema := os.Getenv("MYSQL8_TEST_SCHEMA")
	if addr == "" || user == "" || schema == "" {
		t.Fatal("MYSQL8_TEST_ADDR, MYSQL8_TEST_USER, and MYSQL8_TEST_SCHEMA must be set when MYSQL8_TEST_DSN is set")
	}
	return Config{Target: user + "@" + addr + "/" + schema, User: user, Password: password}
}

func assertMySQLMetadataStates(t *testing.T, snapshot *catalog.MetadataSnapshot) {
	t.Helper()
	for _, kind := range []catalog.MetadataKind{
		catalog.MetadataSchemas,
		catalog.MetadataRelations,
		catalog.MetadataColumnsCurrent,
		catalog.MetadataPrimaryKeys,
		catalog.MetadataForeignKeys,
		catalog.MetadataViews,
		catalog.MetadataProcedures,
		catalog.MetadataFunctions,
		catalog.MetadataIndexes,
		catalog.MetadataTriggers,
	} {
		status, ok := snapshot.Status[kind]
		if !ok || status.State != catalog.MetadataReady {
			t.Errorf("metadata category %s = %+v (present=%t), want ready", kind, status, ok)
		}
	}
	for _, kind := range []catalog.MetadataKind{
		catalog.MetadataColumnsAll,
		catalog.MetadataDependencies,
		catalog.MetadataGenerators,
		catalog.MetadataDomains,
		catalog.MetadataExceptions,
		catalog.MetadataSystemRelations,
	} {
		status, ok := snapshot.Status[kind]
		if !ok || status.State != catalog.MetadataUnsupported {
			t.Errorf("metadata category %s = %+v (present=%t), want unsupported", kind, status, ok)
		}
	}
}

func assertMySQLSchemaScope(t *testing.T, cache *catalog.Cache, schema string) {
	t.Helper()
	if cache.DefaultSchema != schema {
		t.Fatalf("DefaultSchema = %q, want %q", cache.DefaultSchema, schema)
	}
	if got, want := cache.Schemas, map[string]string{strings.ToUpper(schema): schema}; !reflect.DeepEqual(got, want) {
		t.Errorf("Schemas = %#v, want only %#v", got, want)
	}
	wantRelations := []string{"composite_pk", "describe_probe", "no_key", "parent_rows", "unique_nonnull", "v_describe_probe"}
	if got := cache.SortedTables(); !reflect.DeepEqual(got, wantRelations) {
		t.Errorf("connected-schema relations = %q, want seeded relations %q", got, wantRelations)
	}
	if _, ok := cache.SchemaTables["MDE_OTHER"]; ok {
		t.Errorf("relations leaked under other schema: %#v", cache.SchemaTables["MDE_OTHER"])
	}
	if _, ok := cache.SortedTablesByDBName("mde_other"); ok {
		t.Error("other schema is present in the published relation cache")
	}

	relations := make(map[string]struct{}, len(wantRelations))
	for _, relation := range wantRelations {
		relations[relation] = struct{}{}
	}
	if got, want := len(cache.ColumnsByID), len(wantRelations); got != want {
		t.Errorf("column relation count = %d, want %d", got, want)
	}
	for relation := range relations {
		columns, ok := cache.ColumnDescsByID(relation)
		if !ok || len(columns) == 0 {
			t.Errorf("columns for connected-schema relation %q are absent", relation)
			continue
		}
		for _, column := range columns {
			if column == nil || column.Schema != schema || column.Table != relation {
				t.Errorf("column for %q has wrong identity: %+v", relation, column)
			}
		}
	}
	for id, columns := range cache.ColumnsByID {
		matched := false
		for relation := range relations {
			if id == (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: relation}).Key() {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("columns were published for an unexpected relation identity %q", id)
		}
		for _, column := range columns {
			if column == nil || column.Schema != schema {
				t.Errorf("column identity %q belongs to another schema: %+v", id, column)
			}
		}
	}
	for key, columns := range cache.ColumnsWithParent {
		if !strings.HasPrefix(key, strings.ToUpper(schema)+"\t") {
			t.Errorf("merged columns have an unexpected schema key %q", key)
		}
		for _, column := range columns {
			if column == nil || column.Schema != schema {
				t.Errorf("merged column under %q belongs to another schema: %+v", key, column)
			}
		}
	}

	if cache.Catalog == nil {
		t.Fatal("extended MySQL catalog is nil")
	}
	for name, view := range cache.Catalog.ViewsByID {
		if view == nil || view.Schema != schema {
			t.Errorf("view %q has wrong schema: %+v", name, view)
		}
	}
	for name, procedure := range cache.Catalog.ProceduresByID {
		if procedure == nil || procedure.Schema != schema {
			t.Errorf("procedure %q has wrong schema: %+v", name, procedure)
		}
	}
	for name, function := range cache.Catalog.FunctionsByID {
		if function == nil || function.Schema != schema {
			t.Errorf("function %q has wrong schema: %+v", name, function)
		}
	}
	for name, trigger := range cache.Catalog.TriggersByID {
		if trigger == nil || trigger.Schema != schema {
			t.Errorf("trigger %q has wrong schema: %+v", name, trigger)
		}
	}
	for name, index := range cache.Catalog.IndexesByID {
		if index == nil || index.Schema != schema {
			t.Errorf("index %q has wrong schema: %+v", name, index)
		}
	}

	for key := range cache.PrimaryKeyColumns {
		if !strings.HasPrefix(key, strings.ToUpper(schema)+"\t") {
			t.Errorf("primary key is outside connected schema: %q", key)
		}
	}
	for id, columns := range cache.PrimaryKeysByID {
		found := false
		for relation := range relations {
			if id == (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: relation}).Key() {
				found = true
				break
			}
		}
		if !found || len(columns) == 0 {
			t.Errorf("primary key has an unexpected relation identity %q: %v", id, columns)
		}
	}
	for source, targets := range cache.ForeignKeys {
		if !strings.HasPrefix(source, strings.ToUpper(schema)+"\t") {
			t.Errorf("foreign-key source is outside connected schema: %q", source)
		}
		for target, keys := range targets {
			if !strings.HasPrefix(target, strings.ToUpper(schema)+"\t") {
				t.Errorf("foreign-key target is outside connected schema: %q", target)
			}
			for _, key := range keys {
				if key == nil {
					t.Errorf("nil foreign key from %q to %q", source, target)
					continue
				}
				for _, pair := range *key {
					if len(pair) != 2 || pair[0] == nil || pair[1] == nil || pair[0].Schema != schema || pair[1].Schema != schema {
						t.Errorf("foreign key from %q to %q has wrong endpoint schema: %#v", source, target, pair)
					}
				}
			}
		}
	}
}

func assertMySQLNativeColumnKeys(t *testing.T, ctx context.Context, cache *catalog.Cache, db *sql.DB) {
	t.Helper()
	for _, test := range []struct {
		table, column, key string
	}{
		{table: "describe_probe", column: "id", key: "PRI"},
		{table: "unique_nonnull", column: "tenant_id", key: "PRI"},
		{table: "describe_probe", column: "parent_id", key: "MUL"},
		{table: "no_key", column: "label", key: ""},
	} {
		column, ok := cache.Column(test.table, test.column)
		if !ok {
			t.Errorf("column %s.%s is absent", test.table, test.column)
			continue
		}
		var native string
		err := db.QueryRowContext(ctx, `SELECT COLUMN_KEY FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, cache.DefaultSchema, test.table, test.column).Scan(&native)
		if err != nil {
			t.Errorf("read native COLUMN_KEY for %s.%s: %v", test.table, test.column, err)
			continue
		}
		if column.Key != native {
			t.Errorf("merged column %s.%s Key = %q, direct information_schema value = %q", test.table, test.column, column.Key, native)
		}
		if column.Key != test.key {
			t.Errorf("column %s.%s Key = %q, want native value %q; descriptor = %+v", test.table, test.column, column.Key, test.key, column)
		}
	}
	seen := make(map[string]bool)
	for _, table := range cache.SortedTables() {
		columns, ok := cache.ColumnDescs(table)
		if !ok {
			t.Errorf("merged columns are absent for relation %q", table)
			continue
		}
		for _, column := range columns {
			var native string
			err := db.QueryRowContext(ctx, `SELECT COLUMN_KEY FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, cache.DefaultSchema, table, column.Name).Scan(&native)
			if err != nil {
				t.Errorf("read native COLUMN_KEY for %s.%s: %v", table, column.Name, err)
				continue
			}
			seen[native] = true
			if column.Key != native {
				t.Errorf("merged column %s.%s Key = %q, direct information_schema value = %q", table, column.Name, column.Key, native)
			}
			if native != "PRI" && native != "UNI" && native != "MUL" && native != "" {
				t.Errorf("native COLUMN_KEY for %s.%s = %q, want one of PRI/UNI/MUL/empty", table, column.Name, native)
			}
		}
	}
	for _, key := range []string{"PRI", "MUL", ""} {
		if !seen[key] {
			t.Errorf("live MySQL column snapshot has no native %q COLUMN_KEY value", key)
		}
	}
	if !seen["UNI"] {
		t.Log("fixture seed has no native UNI COLUMN_KEY: its only non-primary unique index is InnoDB's clustered key and MySQL reports PRI")
	}
}

func assertMySQLWrongSchemaDetailsDoNotQuery(t *testing.T, ctx context.Context, provider *mysqlProvider, lease *metadata.Lease) {
	t.Helper()
	if provider == nil || provider.conn == nil {
		t.Fatal("live provider connection was not captured")
	}
	// Saturate the provider's one-connection pool. An implementation that tries
	// to issue SHOW CREATE before rejecting the mismatched schema cannot get a
	// connection and will hit the deadline instead of returning ErrUnsupported.
	provider.conn.SetMaxOpenConns(1)
	provider.conn.SetMaxIdleConns(0)
	held, err := provider.conn.Conn(ctx)
	if err != nil {
		t.Fatalf("hold provider connection: %v", err)
	}
	defer held.Close()

	requestCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for _, name := range []string{"describe_probe", "other_probe"} {
		_, err = lease.Detail(requestCtx, metadata.Request{
			Object: catalog.ObjectID{Schema: "mde_other", Kind: catalog.ObjectKindTable, Name: name},
			Facet:  "ddl", Consistency: metadata.Committed,
		})
		if !errors.Is(err, metadata.ErrUnsupported) {
			t.Fatalf("wrong-schema Detail(mde_other.%s) error = %v, want immediate metadata.ErrUnsupported without querying", name, err)
		}
	}
}

func assertMySQLCreateDetails(t *testing.T, ctx context.Context, provider *mysqlProvider, lease *metadata.Lease, schema string) {
	t.Helper()
	if provider == nil || provider.conn == nil {
		t.Fatal("live provider connection was not captured")
	}
	for _, object := range []struct {
		kind  catalog.ObjectKind
		name  string
		verb  string
		label string
	}{
		{kind: catalog.ObjectKindTable, name: "parent_rows", verb: "TABLE", label: "Create Table"},
		{kind: catalog.ObjectKindTable, name: "describe_probe", verb: "TABLE", label: "Create Table"},
		{kind: catalog.ObjectKindTable, name: "unique_nonnull", verb: "TABLE", label: "Create Table"},
		{kind: catalog.ObjectKindTable, name: "no_key", verb: "TABLE", label: "Create Table"},
		{kind: catalog.ObjectKindTable, name: "composite_pk", verb: "TABLE", label: "Create Table"},
		{kind: catalog.ObjectKindView, name: "v_describe_probe", verb: "VIEW", label: "Create View"},
		{kind: catalog.ObjectKindProcedure, name: "sp_probe_echo", verb: "PROCEDURE", label: "Create Procedure"},
		{kind: catalog.ObjectKindFunction, name: "fn_probe_increment", verb: "FUNCTION", label: "Create Function"},
		{kind: catalog.ObjectKindTrigger, name: "tr_describe_probe_bu", verb: "TRIGGER", label: "SQL Original Statement"},
	} {
		request := metadata.Request{
			Object: catalog.ObjectID{Schema: schema, Kind: object.kind, Name: object.name},
			Facet:  "ddl", Consistency: metadata.Committed,
		}
		detail, err := lease.Detail(ctx, request)
		if err != nil {
			t.Errorf("Detail(%s %s ddl) error = %v", object.kind, object.name, err)
			continue
		}
		got, err := liveMySQLShowCreate(ctx, provider.conn, schema, object.name, object.verb, object.label)
		if err != nil {
			t.Errorf("SHOW CREATE %s %s failed: %v", object.verb, object.name, err)
			continue
		}
		if detail.Text != got {
			t.Errorf("Detail(%s %s ddl) = %q, want exact SHOW CREATE %s output %q", object.kind, object.name, detail.Text, object.verb, got)
		}
	}
}

func liveMySQLShowCreate(ctx context.Context, db *sql.DB, schema, name, verb, ddlLabel string) (string, error) {
	statement := "SHOW CREATE " + verb + " " + mysqlLiveQuoteIdentifier(schema) + "." + mysqlLiveQuoteIdentifier(name)
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	labels, err := rows.Columns()
	if err != nil {
		return "", err
	}
	ddlIndex := -1
	for i, label := range labels {
		if label == ddlLabel {
			ddlIndex = i
			break
		}
	}
	if ddlIndex < 0 {
		return "", fmt.Errorf("SHOW CREATE has no %q column (columns %q)", ddlLabel, labels)
	}
	values := make([]any, len(labels))
	destinations := make([]any, len(labels))
	for i := range values {
		destinations[i] = &values[i]
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", errors.New("SHOW CREATE returned no rows")
	}
	if err := rows.Scan(destinations...); err != nil {
		return "", err
	}
	if rows.Next() {
		return "", errors.New("SHOW CREATE returned multiple rows")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch value := values[ddlIndex].(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	default:
		return "", fmt.Errorf("SHOW CREATE %q column has type %T", ddlLabel, values[ddlIndex])
	}
}

func mysqlLiveQuoteIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}
