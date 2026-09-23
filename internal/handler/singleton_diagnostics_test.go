package handler

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func singletonDiagnosticCache() *database.DBCache {
	index := &database.IndexDesc{
		Name: "PK_ORDERS", RelationName: "ORDERS", Columns: []string{"ID", "LINE_NO"},
		Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true},
		ConstraintName: sql.NullString{String: "PK_ORDERS", Valid: true},
	}
	return &database.DBCache{
		SchemaTables: map[string][]string{"": {"ORDERS"}},
		ColumnsWithParent: map[string][]*database.ColumnDesc{
			"\tORDERS": {
				{ColumnBase: database.ColumnBase{Table: "ORDERS", Name: "ID"}, Type: "INTEGER", Key: "YES"},
				{ColumnBase: database.ColumnBase{Table: "ORDERS", Name: "LINE_NO"}, Type: "INTEGER", Key: "YES"},
			},
		},
		Catalog: &database.CatalogCache{
			Indexes:        map[string]*database.IndexDesc{"PK_ORDERS": index},
			IndexesByTable: map[string][]*database.IndexDesc{"ORDERS": {index}},
		},
	}
}

func TestSingletonDiagnosticCatalogAndRanges(t *testing.T) {
	tests := []struct {
		name   string
		where  string
		mutate func(*database.DBCache)
		warn   bool
	}{
		{name: "partial primary key", where: "ID = 1", warn: true},
		{name: "complete primary key", where: "ID = 1 AND LINE_NO = 2"},
		{name: "standalone unique index", where: "ID = 1", mutate: func(c *database.DBCache) {
			idx := c.Catalog.Indexes["PK_ORDERS"]
			idx.ConstraintName = sql.NullString{}
			idx.Columns = []string{"ID"}
		}},
		{name: "inactive index", where: "ID = 1 AND LINE_NO = 2", warn: true, mutate: func(c *database.DBCache) {
			c.Catalog.Indexes["PK_ORDERS"].Active.Bool = false
		}},
		{name: "nonunique index", where: "ID = 1 AND LINE_NO = 2", warn: true, mutate: func(c *database.DBCache) {
			c.Catalog.Indexes["PK_ORDERS"].Unique.Bool = false
		}},
		{name: "unknown active flag", where: "ID = 1 AND LINE_NO = 2", mutate: func(c *database.DBCache) {
			c.Catalog.Indexes["PK_ORDERS"].Active.Valid = false
		}},
		{name: "missing catalog", where: "ID > 1", mutate: func(c *database.DBCache) { c.Catalog = nil }},
		{name: "no indexes", where: "ID = 1", warn: true, mutate: func(c *database.DBCache) {
			c.Catalog.Indexes = nil
			c.Catalog.IndexesByTable = nil
		}},
		{name: "view", where: "ID > 1", mutate: func(c *database.DBCache) {
			c.Catalog.Views = map[string]*database.ViewDesc{"ORDERS": {Name: "ORDERS"}}
		}},
		{name: "expression index metadata", where: "ID = 1 AND LINE_NO = 2", mutate: func(c *database.DBCache) {
			c.Catalog.Indexes["PK_ORDERS"].Expression = sql.NullString{String: "ID + LINE_NO", Valid: true}
			c.Catalog.Indexes["PK_ORDERS"].Columns = nil
		}},
		{name: "wrong case relation index", where: "ID = 1 AND LINE_NO = 2", warn: true, mutate: func(c *database.DBCache) {
			c.Catalog.Indexes["PK_ORDERS"].RelationName = "Orders"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := singletonDiagnosticCache()
			if tt.mutate != nil {
				tt.mutate(cache)
			}
			text := "/*😀*/ CREATE PROCEDURE P RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM ORDERS WHERE " + tt.where + " INTO :RESULT; END"
			found := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
				text: text, variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, cacheSnapshot: snapshotDiagnosticCatalog(cache),
			})
			want := 0
			if tt.warn {
				want = 1
			}
			if len(found) != want {
				t.Fatalf("diagnostics = %+v, want %d", found, want)
			}
			if len(found) > 0 {
				start := utf16Units(text[:strings.Index(text, "SELECT")])
				if diagnosticCode(found[0]) != "interbase-singleton-select" || diagnosticSource(found[0]) != "sqls" || found[0].Severity != 2 ||
					found[0].Range.Start.Line != 0 || found[0].Range.Start.Character != start || found[0].Range.End.Character != start+6 {
					t.Fatalf("incorrect singleton diagnostic: %+v", found[0])
				}
			}
		})
	}
}

func TestSingletonDiagnosticSnapshotOwnsKeys(t *testing.T) {
	cache := singletonDiagnosticCache()
	catalog := snapshotDiagnosticCatalog(cache)
	cache.Catalog.Indexes["PK_ORDERS"].Columns[1] = "ID"
	text := "CREATE PROCEDURE P RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM ORDERS WHERE ID = 1 INTO :RESULT; END"
	found := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
		text: text, variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, cacheSnapshot: catalog,
	})
	if len(found) != 1 || diagnosticCode(found[0]) != "interbase-singleton-select" {
		t.Fatalf("catalog mutation changed snapshot diagnostic: %+v", found)
	}
}

func TestPublishSingletonDiagnosticClearsOnEdit(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	uri := "file:///singleton.sql"
	unsafe := "CREATE PROCEDURE P RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM ORDERS INTO :RESULT; END"
	tx.open(t, uri, unsafe, 1)
	tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	snapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("missing document snapshot")
	}
	snapshot.cacheSnapshot = snapshotDiagnosticCatalog(singletonDiagnosticCache())
	tx.server.publishDiagnosticsSnapshot(tx.ctx, tx.serverConn, snapshot, diagnosticsForSnapshot(snapshot))
	n := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(n.Diagnostics) != 1 || diagnosticCode(n.Diagnostics[0]) != "interbase-singleton-select" {
		t.Fatalf("missing singleton warning: %+v", n)
	}

	safe := strings.Replace(unsafe, "INTO", "WHERE ID = 1 AND LINE_NO = 1 INTO", 1)
	tx.change(t, uri, safe, 2)
	tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	snapshot, _ = tx.server.diagnosticsSnapshot(uri)
	snapshot.cacheSnapshot = snapshotDiagnosticCatalog(singletonDiagnosticCache())
	tx.server.publishDiagnosticsSnapshot(tx.ctx, tx.serverConn, snapshot, diagnosticsForSnapshot(snapshot))
	n = tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	if len(n.Diagnostics) != 0 {
		t.Fatalf("singleton warning not cleared: %+v", n)
	}
	tx.call(t, "textDocument/didClose", lsp.DidCloseTextDocumentParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}})
}
