package database

import "github.com/sqls-server/sqls/catalog"

// MetadataPlanFor is the exported-in-internal-package adaptation of
// metadataPlanFor: it lets callers outside this package (the future LSP host,
// and later tasks) build a catalog.MetadataPlan from a DBRepository without
// reaching into sqls internals.
func MetadataPlanFor(repo DBRepository) catalog.MetadataPlan {
	return metadataPlanFor(repo)
}
