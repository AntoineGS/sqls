package language

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// TestCacheDiagnosticCatalogHoldsNoCopy structurally proves
// cacheDiagnosticCatalog cannot be a full-copy implementation: it has
// exactly one field, a shared *catalog.Cache pointer. A naive
// implementation that eagerly copied every table's columns and unique
// keys (the pattern this task removes) would need its own map fields to
// hold that copy, which would fail this assertion.
func TestCacheDiagnosticCatalogHoldsNoCopy(t *testing.T) {
	typ := reflect.TypeOf(cacheDiagnosticCatalog{})
	if typ.NumField() != 1 {
		t.Fatalf("cacheDiagnosticCatalog has %d fields, want exactly 1 (a shared *catalog.Cache pointer); an extra field would mean it retains its own copy of catalog data", typ.NumField())
	}
	field := typ.Field(0)
	if field.Type != reflect.TypeOf((*catalog.Cache)(nil)) {
		t.Fatalf("cacheDiagnosticCatalog.%s has type %s, want *catalog.Cache", field.Name, field.Type)
	}
}

// TestCacheDiagnosticCatalogSharesUnderlyingCache proves two catalogs built
// from the same *catalog.Cache read through to the very same cache data
// rather than each capturing an independent copy at construction time: a
// mutation made to the cache after both adapters exist is visible to both.
// A per-call full-copy implementation could also pass a test that only
// checked freshness once; this test additionally checks a second,
// independently constructed adapter observes the same live data, which a
// copying implementation could only do by rebuilding its whole copy on
// every lookup.
func TestCacheDiagnosticCatalogSharesUnderlyingCache(t *testing.T) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "EMPLOYEE"}.Key()
	cache := &catalog.Cache{
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			key: {{ColumnBase: catalog.ColumnBase{Name: "ID"}, Type: "INTEGER"}},
		},
	}
	table := sqlsymbol.Name{Text: "EMPLOYEE"}

	first := cacheDiagnosticCatalog{cache: cache}
	second := cacheDiagnosticCatalog{cache: cache}
	if first.cache != second.cache {
		t.Fatal("two adapters built from the same cache do not share the same pointer")
	}

	columns, ok := first.Columns(table)
	if !ok || len(columns) != 1 || columns[0].Name != "ID" {
		t.Fatalf("first.Columns before mutation = %+v, %v", columns, ok)
	}

	cache.ColumnsByID[key] = append(cache.ColumnsByID[key], &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Name: "NAME"}, Type: "VARCHAR(40)"})

	columns, ok = second.Columns(table)
	if !ok || len(columns) != 2 || columns[1].Name != "NAME" {
		t.Fatalf("second.Columns after mutation = %+v, %v; a second adapter that held its own copy would not see the mutation", columns, ok)
	}
}

// TestCacheDiagnosticCatalogColumnsResolvesExactIdentity is the
// quoted-vs-unquoted regression test for the exact-identity fix: two
// relations whose names differ only by quoting/case must never share
// columns, even though they fold to the same upper-cased key that the
// legacy ColumnDescs/ColumnDatabase accessors use.
func TestCacheDiagnosticCatalogColumnsResolvesExactIdentity(t *testing.T) {
	quotedKey := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Orders"}.Key()
	unquotedKey := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ORDERS"}.Key()
	cache := &catalog.Cache{
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			quotedKey:   {{ColumnBase: catalog.ColumnBase{Name: "CustomerId"}, Type: "INTEGER"}},
			unquotedKey: {{ColumnBase: catalog.ColumnBase{Name: "ID"}, Type: "INTEGER"}, {ColumnBase: catalog.ColumnBase{Name: "TOTAL"}, Type: "DECIMAL"}},
		},
	}
	view := cacheDiagnosticCatalog{cache: cache}

	quoted := sqlsymbol.Name{Text: "Orders", Quoted: true}
	quotedColumns, ok := view.Columns(quoted)
	if !ok || len(quotedColumns) != 1 || quotedColumns[0].Name != "CustomerId" {
		t.Fatalf("quoted Orders columns = %+v, %v; want [{CustomerId}], true", quotedColumns, ok)
	}

	unquoted := sqlsymbol.Name{Text: "orders"}
	unquotedColumns, ok := view.Columns(unquoted)
	if !ok || len(unquotedColumns) != 2 || unquotedColumns[0].Name != "ID" || unquotedColumns[1].Name != "TOTAL" {
		t.Fatalf("unquoted orders columns = %+v, %v; want [{ID} {TOTAL}], true", unquotedColumns, ok)
	}
}

// TestCacheDiagnosticCatalogColumnsPopulatedByIDMissIsUnknownNotFolded is the
// regression test for the round-1 reintroduction: once ColumnsByID has been
// populated at all (here, with only the unquoted upper-cased key present),
// a quoted lookup that misses ColumnsByID must report unknown outright, not
// fall through to the folded ColumnDescs accessor and silently return a
// different relation's columns.
func TestCacheDiagnosticCatalogColumnsPopulatedByIDMissIsUnknownNotFolded(t *testing.T) {
	unquotedKey := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ORDERS"}.Key()
	cache := &catalog.Cache{
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			unquotedKey: {{ColumnBase: catalog.ColumnBase{Name: "ID"}, Type: "INTEGER"}},
		},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			"\tORDERS": {{ColumnBase: catalog.ColumnBase{Name: "ID"}, Type: "INTEGER"}},
		},
	}
	view := cacheDiagnosticCatalog{cache: cache}

	quoted := sqlsymbol.Name{Text: "Orders", Quoted: true}
	columns, ok := view.Columns(quoted)
	if ok {
		t.Fatalf("quoted Orders columns = %+v, ok=true; want ok=false: ColumnsByID is populated (for a different exact identity), so a miss must mean \"no such relation,\" not fall back to the folded accessor and return ORDERS's columns", columns)
	}
}

// TestDiagnosticCatalogReadyRequiresColumnsReady relocates the handler-level
// assertion (formerly TestDiagnosticsSnapshotWaitsForColumnsReadyBeforeCatalogBuild)
// that a catalog view is never attached to diagnostics before the cache's
// column metadata is ready, straight against the gate that now owns that
// decision: diagnosticCatalogReady.
func TestDiagnosticCatalogReadyRequiresColumnsReady(t *testing.T) {
	cache := &catalog.Cache{
		Catalog:  &catalog.CatalogCache{},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{},
	}
	snapshot := &catalog.MetadataSnapshot{Cache: cache}
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	if diagnosticCatalogReady(snapshot, variant) {
		t.Fatal("catalog reported ready before columns metadata became ready")
	}
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataReady
	if !diagnosticCatalogReady(snapshot, variant) {
		t.Fatal("catalog not reported ready after columns metadata became ready")
	}
}

// TestCacheDiagnosticCatalogUniqueKeysRequiresCompleteCatalogInputs relocates
// the handler-level TestDiagnosticUniqueKeysRequiresCompleteCatalogInputs
// (which exercised the now-deleted internal/handler.diagnosticUniqueKeys)
// against cacheDiagnosticCatalog.UniqueKeys, the function that now owns this
// gating.
func TestCacheDiagnosticCatalogUniqueKeysRequiresCompleteCatalogInputs(t *testing.T) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key()
	index := &catalog.IndexDesc{
		Name: "UQ_T", RelationName: "T", Columns: []string{"ID"},
		Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true},
	}
	cache := &catalog.Cache{
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			key: {{ColumnBase: catalog.ColumnBase{Name: "ID"}, Type: "INTEGER"}},
		},
		Catalog: &catalog.CatalogCache{
			Views:          map[string]*catalog.ViewDesc{},
			Indexes:        map[string]*catalog.IndexDesc{"UQ_T": index},
			IndexesByTable: map[string][]*catalog.IndexDesc{"T": {index}},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataLoading,
			catalog.MetadataIndexes:        catalog.MetadataReady,
		},
	}
	table := sqlsymbol.Name{Text: "T"}
	view := cacheDiagnosticCatalog{cache: cache}

	if _, known := view.UniqueKeys(table); known {
		t.Fatal("incomplete view classification cannot prove table keys")
	}
	cache.Metadata[catalog.MetadataViews] = catalog.MetadataReady
	cache.Metadata[catalog.MetadataIndexes] = catalog.MetadataLoading
	if _, known := view.UniqueKeys(table); known {
		t.Fatal("loading indexes cannot prove a complete unique-key set")
	}
	cache.Metadata[catalog.MetadataIndexes] = catalog.MetadataReady
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataFailed
	if _, known := view.UniqueKeys(table); known {
		t.Fatal("failed columns cannot prove a complete unique-key set")
	}
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataReady
	keys, known := view.UniqueKeys(table)
	if !known || len(keys) != 1 || len(keys[0]) != 1 || keys[0][0] != "ID" {
		t.Fatalf("complete catalog keys = %v, known=%v; want [[ID]], true", keys, known)
	}
	cache.Catalog.Indexes = map[string]*catalog.IndexDesc{}
	cache.Catalog.IndexesByTable = map[string][]*catalog.IndexDesc{}
	keys, known = view.UniqueKeys(table)
	if !known || len(keys) != 0 {
		t.Fatalf("ready-empty indexes keys = %v, known=%v; want empty, true", keys, known)
	}
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataFailed
	cache.Metadata[catalog.MetadataColumnsAll] = catalog.MetadataReady
	if _, known := view.UniqueKeys(table); !known {
		t.Fatal("all-columns readiness should satisfy the columns requirement")
	}
}
