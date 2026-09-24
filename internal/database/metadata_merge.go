package database

import "github.com/sqls-server/sqls/metadata"

// newMetadataCache and mergeMetadata are thin delegates to the shared
// metadata package's merge machinery, kept under their original unexported
// names so existing call sites and tests in this package need no edit.
func newMetadataCache() *DBCache { return metadata.NewCache() }

func mergeMetadata(base *DBCache, kind MetadataKind, patch MetadataPatch) (*DBCache, error) {
	return metadata.Merge(base, kind, patch)
}
