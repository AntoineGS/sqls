// This file proves the public language package can be constructed using
// only SQLs's other public packages (catalog, dialect, metadata) and the
// standard library — never an internal SQLs package, an LSP transport
// type, or a host-specific DTO. Its imports are the enforcement: this file
// would fail to compile if language.Context or language.Request required
// anything else.
package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

type fakeDetailReader struct{}

func (fakeDetailReader) Detail(context.Context, metadata.Request) (metadata.Detail, error) {
	return metadata.Detail{}, nil
}

func TestPublicPackageConstructsFromPlanAPublicTypesOnly(t *testing.T) {
	snapshot := &catalog.MetadataSnapshot{Cache: &catalog.Cache{}}
	req := language.Request{
		Document: language.Document{
			URI:     "memory:one",
			Text:    "SELECT * FROM T",
			Version: 1,
			Binding: 1,
		},
		Context: language.Context{
			Key:             metadata.Key{Engine: "interbase", URL: "db1"},
			Incarnation:     1,
			Snapshot:        snapshot,
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
			Details:         fakeDetailReader{},
		},
	}
	if req.Context.Snapshot.Cache == nil {
		t.Fatal("snapshot cache not constructed")
	}
	if req.Context.Details == nil {
		t.Fatal("details reader not wired")
	}
}

func TestLeaseSatisfiesDetailReader(t *testing.T) {
	var _ language.DetailReader = (*metadata.Lease)(nil)
}
