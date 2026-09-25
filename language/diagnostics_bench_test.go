package language_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

// BenchmarkDiagnosticsReusesSharedCatalog is the language-package
// replacement for the old handler-level BenchmarkMetadataDiagnosticCatalogReuse:
// it repeatedly runs Service.Diagnostics against one large shared
// *catalog.Cache to demonstrate that per-call cost stays flat rather than
// growing with the number of prior calls, which a per-call full-copy
// catalog adapter (the pattern Task B2 removed) would not sustain.
func BenchmarkDiagnosticsReusesSharedCatalog(b *testing.B) {
	const tableCount = 256
	cache := &catalog.Cache{
		ColumnsByID: make(map[string][]*catalog.ColumnDesc, tableCount),
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
		},
	}
	for i := 0; i < tableCount; i++ {
		table := "TABLE_" + strconv.Itoa(i)
		key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: table}.Key()
		cache.ColumnsByID[key] = []*catalog.ColumnDesc{
			{ColumnBase: catalog.ColumnBase{Table: table, Name: "VALUE"}, Type: "VARCHAR(10)"},
		}
	}
	snap := &catalog.MetadataSnapshot{Cache: cache}
	// TABLE_0/TABLE_1 are among the tableCount tables seeded above, so this
	// insert's width check actually calls cacheDiagnosticCatalog.Columns
	// against the shared cache on every benchmark iteration; DST/SRC (an
	// earlier version of this benchmark) named tables outside the seeded
	// set, so diagnosticCatalogReady never attached a catalog and every
	// iteration measured the catalog-free path instead.
	req := language.Request{
		Document: language.Document{
			URI:     "memory:bench",
			Text:    "CREATE PROCEDURE P AS DECLARE VARIABLE LOCAL_VALUE VARCHAR(10); BEGIN INSERT INTO TABLE_0 (VALUE) SELECT TABLE_1.VALUE FROM TABLE_1; END",
			Version: 1,
			Binding: 1,
		},
		Context: language.Context{
			Snapshot:        snap,
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	svc := language.NewService()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Diagnostics(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}
