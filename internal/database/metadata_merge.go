package database

import (
	"errors"
	"fmt"
	"strings"
)

var errNilMetadataPatch = errors.New("metadata patch cache is nil")

// newMetadataCache returns the empty, mutable input to one metadata generation.
func newMetadataCache() *DBCache {
	return &DBCache{
		Schemas:           make(map[string]string),
		SchemaTables:      make(map[string][]string),
		ColumnsWithParent: make(map[string][]*ColumnDesc),
		ForeignKeys:       make(map[string]map[string][]*ForeignKey),
		Metadata:          make(map[MetadataKind]MetadataState),
		PrimaryKeyColumns: make(map[string]map[string]struct{}),
	}
}

// mergeMetadata publishes one successful category without changing base. The
// fragment is transferred to the resulting cache; only PK enrichment copies
// descriptors because that operation changes previously published values.
func mergeMetadata(base *DBCache, kind MetadataKind, patch MetadataPatch) (*DBCache, error) {
	if base == nil {
		return nil, errors.New("base metadata cache is nil")
	}
	if patch.Cache == nil {
		return nil, errNilMetadataPatch
	}
	if patch.Count < 0 || patch.Queries < 0 {
		return nil, errors.New("metadata patch counts must not be negative")
	}
	if !knownMetadataKind(kind) {
		return nil, fmt.Errorf("unknown metadata kind %q", kind)
	}

	next := *base
	next.Metadata = cloneMap(base.Metadata)
	next.Metadata[kind] = MetadataReady
	fragment := patch.Cache

	switch kind {
	case MetadataSchemas:
		next.defaultSchema = fragment.defaultSchema
		next.Schemas = cloneMap(fragment.Schemas)
	case MetadataRelations:
		next.SchemaTables = cloneStringSlices(fragment.SchemaTables)
	case MetadataColumnsCurrent, MetadataColumnsAll:
		// The all-schema result supersedes current-schema data regardless of
		// completion order, while both statuses remain independently ready.
		if kind == MetadataColumnsCurrent && base.MetadataReady(MetadataColumnsAll) {
			break
		}
		next.ColumnsWithParent = cloneColumnSlices(fragment.ColumnsWithParent)
		if base.MetadataReady(MetadataPrimaryKeys) {
			next.ColumnsWithParent = columnsWithPrimaryKeys(next.ColumnsWithParent, base.PrimaryKeyColumns)
		}
	case MetadataPrimaryKeys:
		next.PrimaryKeyColumns = cloneNestedSetMap(fragment.PrimaryKeyColumns)
		if base.ColumnsReady() {
			next.ColumnsWithParent = columnsWithPrimaryKeys(base.ColumnsWithParent, next.PrimaryKeyColumns)
		}
	case MetadataForeignKeys:
		next.ForeignKeys = cloneForeignKeyMap(fragment.ForeignKeys)
	case MetadataViews, MetadataProcedures, MetadataGenerators, MetadataDomains,
		MetadataFunctions, MetadataIndexes, MetadataTriggers:
		catalog := cloneCatalogHeader(base.Catalog)
		fragmentCatalog := fragment.Catalog
		switch kind {
		case MetadataViews:
			catalog.Views = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*ViewDesc { return c.Views })
		case MetadataProcedures:
			catalog.Procedures = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*ProcedureDesc { return c.Procedures })
		case MetadataGenerators:
			catalog.Generators = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*GeneratorDesc { return c.Generators })
		case MetadataDomains:
			catalog.Domains = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*DomainDesc { return c.Domains })
		case MetadataFunctions:
			catalog.Functions = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*FunctionDesc { return c.Functions })
		case MetadataIndexes:
			catalog.Indexes = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*IndexDesc { return c.Indexes })
			catalog.IndexesByTable = indexGroups(catalog.Indexes)
		case MetadataTriggers:
			catalog.Triggers = mapOrEmpty(fragmentCatalog, func(c *CatalogCache) map[string]*TriggerDesc { return c.Triggers })
			catalog.TriggersByTable = triggerGroups(catalog.Triggers)
		}
		next.Catalog = catalog
	}
	return &next, nil
}

func mapOrEmpty[V any](catalog *CatalogCache, get func(*CatalogCache) map[string]V) map[string]V {
	if catalog == nil || get(catalog) == nil {
		return make(map[string]V)
	}
	return get(catalog)
}

func knownMetadataKind(kind MetadataKind) bool {
	switch kind {
	case MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataColumnsAll,
		MetadataPrimaryKeys, MetadataForeignKeys, MetadataViews, MetadataProcedures,
		MetadataGenerators, MetadataDomains, MetadataFunctions, MetadataIndexes, MetadataTriggers:
		return true
	default:
		return false
	}
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringSlices(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func cloneColumnSlices(source map[string][]*ColumnDesc) map[string][]*ColumnDesc {
	result := make(map[string][]*ColumnDesc, len(source))
	for key, columns := range source {
		result[key] = append([]*ColumnDesc(nil), columns...)
	}
	return result
}

func cloneNestedSetMap(source map[string]map[string]struct{}) map[string]map[string]struct{} {
	result := make(map[string]map[string]struct{}, len(source))
	for key, values := range source {
		result[key] = cloneMap(values)
	}
	return result
}

func cloneForeignKeyMap(source map[string]map[string][]*ForeignKey) map[string]map[string][]*ForeignKey {
	result := make(map[string]map[string][]*ForeignKey, len(source))
	for table, references := range source {
		copied := make(map[string][]*ForeignKey, len(references))
		for refTable, keys := range references {
			copied[refTable] = append([]*ForeignKey(nil), keys...)
		}
		result[table] = copied
	}
	return result
}

func cloneCatalogHeader(source *CatalogCache) *CatalogCache {
	if source == nil {
		return &CatalogCache{}
	}
	copy := *source
	return &copy
}

func columnsWithPrimaryKeys(columns map[string][]*ColumnDesc, primaryKeys map[string]map[string]struct{}) map[string][]*ColumnDesc {
	result := make(map[string][]*ColumnDesc, len(columns))
	for table, descriptors := range columns {
		keys := primaryKeys[table]
		copied := make([]*ColumnDesc, len(descriptors))
		for i, descriptor := range descriptors {
			if descriptor == nil {
				continue
			}
			column := *descriptor
			column.Key = "NO"
			for key := range keys {
				if strings.EqualFold(key, column.Name) {
					column.Key = "YES"
					break
				}
			}
			copied[i] = &column
		}
		result[table] = copied
	}
	return result
}

func indexGroups(indexes map[string]*IndexDesc) map[string][]*IndexDesc {
	groups := make(map[string][]*IndexDesc)
	for _, index := range indexes {
		if index == nil || index.RelationName == "" {
			continue
		}
		key := catalogCacheKey(index.RelationName)
		groups[key] = append(groups[key], index)
	}
	return groups
}

func triggerGroups(triggers map[string]*TriggerDesc) map[string][]*TriggerDesc {
	groups := make(map[string][]*TriggerDesc)
	for _, trigger := range triggers {
		if trigger == nil || !trigger.RelationName.Valid || strings.TrimSpace(trigger.RelationName.String) == "" {
			continue
		}
		key := catalogCacheKey(trigger.RelationName.String)
		groups[key] = append(groups[key], trigger)
	}
	return groups
}
