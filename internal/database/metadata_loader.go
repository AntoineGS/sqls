package database

import (
	"context"

	"github.com/sqls-server/sqls/metadata"
)

var (
	ErrMetadataStaleGeneration = metadata.ErrMetadataStaleGeneration
	ErrMetadataAlreadyStarted  = metadata.ErrMetadataAlreadyStarted
	ErrMetadataStopped         = metadata.ErrMetadataStopped
	ErrInvalidMetadataPlan     = metadata.ErrInvalidMetadataPlan
)

// MetadataLoader is a thin, repository-aware wrapper around the neutral
// metadata.Loader: it derives a catalog.MetadataPlan from a DBRepository via
// MetadataPlanFor before delegating scheduling to the shared loader
// implementation. There is exactly one loader implementation; this type adds
// no scheduling logic of its own.
type MetadataLoader struct {
	*metadata.Loader
}

func NewMetadataLoader() *MetadataLoader {
	return &MetadataLoader{Loader: metadata.NewLoader()}
}

// Start derives repo's metadata plan and starts generation with it. Plan
// computation happens outside any loader lock, exactly as it did before this
// loader was extracted, so existing tests about concurrent/lazy planning keep
// their behaviour.
func (l *MetadataLoader) Start(ctx context.Context, generation uint64, repo DBRepository) (*MetadataLoad, error) {
	plan := metadataPlanFor(repo)
	return l.Loader.Start(ctx, generation, plan)
}
