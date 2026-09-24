package metadata

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

var errNilMetadataPatch = errors.New("metadata patch cache is nil")

// NewCache returns the empty, mutable input to one metadata generation. It is
// exported so engine-specific test helpers (for example internal/database's
// behaviour-parity tests) can exercise the same merge machinery the loader
// uses internally, without a second implementation.
func NewCache() *catalog.Cache { return newMetadataCache() }

// Merge publishes one successful category's patch onto base. See
// mergeMetadata for the exact semantics; this is its exported form.
func Merge(base *catalog.Cache, kind catalog.MetadataKind, patch catalog.MetadataPatch) (*catalog.Cache, error) {
	return mergeMetadata(base, kind, patch)
}

// newMetadataCache returns the empty, mutable input to one metadata generation.
func newMetadataCache() *catalog.Cache {
	return &catalog.Cache{
		Schemas:           make(map[string]string),
		SchemaTables:      make(map[string][]string),
		ColumnsWithParent: make(map[string][]*catalog.ColumnDesc),
		ForeignKeys:       make(map[string]map[string][]*catalog.ForeignKey),
		Metadata:          make(map[catalog.MetadataKind]catalog.MetadataState),
		PrimaryKeyColumns: make(map[string]map[string]struct{}),
	}
}

// mergeMetadata publishes one successful category without changing base. The
// fragment is transferred to the resulting cache; only PK enrichment copies
// descriptors because that operation changes previously published values.
func mergeMetadata(base *catalog.Cache, kind catalog.MetadataKind, patch catalog.MetadataPatch) (*catalog.Cache, error) {
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
	if err := validateMetadataPayload(kind, patch.Cache); err != nil {
		return nil, err
	}
	if kind == catalog.MetadataForeignKeys {
		for table, references := range patch.Cache.ForeignKeys {
			for referencedTable, keys := range references {
				for index, key := range keys {
					if err := catalog.ValidateForeignKey(key); err != nil {
						return nil, fmt.Errorf("metadata foreign key %s -> %s at index %d: %w", table, referencedTable, index, err)
					}
				}
			}
		}
	}

	next := *base
	next.Metadata = cloneMap(base.Metadata)
	next.Metadata[kind] = catalog.MetadataReady
	fragment := patch.Cache

	switch kind {
	case catalog.MetadataSchemas:
		next.DefaultSchema = fragment.DefaultSchema
		next.Schemas = cloneMap(fragment.Schemas)
	case catalog.MetadataRelations:
		next.SchemaTables = cloneStringSlices(fragment.SchemaTables)
	case catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll:
		// The all-schema result supersedes current-schema data regardless of
		// completion order, while both statuses remain independently ready.
		if kind == catalog.MetadataColumnsCurrent && base.MetadataReady(catalog.MetadataColumnsAll) {
			break
		}
		next.ColumnsWithParent = cloneColumnSlices(fragment.ColumnsWithParent)
		if base.MetadataReady(catalog.MetadataPrimaryKeys) {
			next.ColumnsWithParent = columnsWithPrimaryKeys(next.ColumnsWithParent, base.PrimaryKeyColumns)
		}
	case catalog.MetadataPrimaryKeys:
		next.PrimaryKeyColumns = cloneNestedSetMap(fragment.PrimaryKeyColumns)
		if base.ColumnsReady() {
			next.ColumnsWithParent = columnsWithPrimaryKeys(base.ColumnsWithParent, next.PrimaryKeyColumns)
		}
	case catalog.MetadataForeignKeys:
		next.ForeignKeys = cloneForeignKeyMap(fragment.ForeignKeys)
	case catalog.MetadataViews, catalog.MetadataProcedures, catalog.MetadataGenerators, catalog.MetadataDomains,
		catalog.MetadataFunctions, catalog.MetadataIndexes, catalog.MetadataTriggers:
		catalogHeader := cloneCatalogHeader(base.Catalog)
		fragmentCatalog := fragment.Catalog
		switch kind {
		case catalog.MetadataViews:
			catalogHeader.Views = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.ViewDesc { return c.Views }))
			catalogHeader.ViewsByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.ViewDesc { return c.ViewsByID }))
		case catalog.MetadataProcedures:
			catalogHeader.Procedures = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.ProcedureDesc { return c.Procedures }))
			catalogHeader.ProceduresByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.ProcedureDesc { return c.ProceduresByID }))
		case catalog.MetadataGenerators:
			catalogHeader.Generators = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.GeneratorDesc { return c.Generators }))
			catalogHeader.GeneratorsByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.GeneratorDesc { return c.GeneratorsByID }))
		case catalog.MetadataDomains:
			catalogHeader.Domains = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.DomainDesc { return c.Domains }))
			catalogHeader.DomainsByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.DomainDesc { return c.DomainsByID }))
		case catalog.MetadataFunctions:
			catalogHeader.Functions = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.FunctionDesc { return c.Functions }))
			catalogHeader.FunctionsByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.FunctionDesc { return c.FunctionsByID }))
		case catalog.MetadataIndexes:
			catalogHeader.Indexes = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.IndexDesc { return c.Indexes }))
			catalogHeader.IndexesByTable = indexGroups(catalogHeader.Indexes)
			catalogHeader.IndexesByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.IndexDesc { return c.IndexesByID }))
		case catalog.MetadataTriggers:
			catalogHeader.Triggers = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.TriggerDesc { return c.Triggers }))
			catalogHeader.TriggersByTable = triggerGroups(catalogHeader.Triggers)
			catalogHeader.TriggersByID = cloneMap(mapOrEmpty(fragmentCatalog, func(c *catalog.CatalogCache) map[string]*catalog.TriggerDesc { return c.TriggersByID }))
		}
		// Objects accumulates exact identities across every category loaded
		// so far (see CatalogCache.RebuildObjects), so it is rebuilt from
		// the union of all *ByID maps after each category's own map is
		// updated above, not just this category's fragment.
		catalogHeader.RebuildObjects(nil)
		next.Catalog = catalogHeader
	}
	return &next, nil
}

func validateMetadataPayload(kind catalog.MetadataKind, fragment *catalog.Cache) error {
	var present bool
	switch kind {
	case catalog.MetadataSchemas:
		present = fragment.Schemas != nil
	case catalog.MetadataRelations:
		present = fragment.SchemaTables != nil
	case catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll:
		present = fragment.ColumnsWithParent != nil
	case catalog.MetadataPrimaryKeys:
		present = fragment.PrimaryKeyColumns != nil
	case catalog.MetadataForeignKeys:
		present = fragment.ForeignKeys != nil
	case catalog.MetadataViews:
		present = fragment.Catalog != nil && fragment.Catalog.Views != nil
	case catalog.MetadataProcedures:
		present = fragment.Catalog != nil && fragment.Catalog.Procedures != nil
	case catalog.MetadataGenerators:
		present = fragment.Catalog != nil && fragment.Catalog.Generators != nil
	case catalog.MetadataDomains:
		present = fragment.Catalog != nil && fragment.Catalog.Domains != nil
	case catalog.MetadataFunctions:
		present = fragment.Catalog != nil && fragment.Catalog.Functions != nil
	case catalog.MetadataIndexes:
		present = fragment.Catalog != nil && fragment.Catalog.Indexes != nil
	case catalog.MetadataTriggers:
		present = fragment.Catalog != nil && fragment.Catalog.Triggers != nil
	}
	if !present {
		return fmt.Errorf("metadata %s payload is missing its category data", kind)
	}
	return nil
}

func mapOrEmpty[V any](catalogCache *catalog.CatalogCache, get func(*catalog.CatalogCache) map[string]V) map[string]V {
	if catalogCache == nil || get(catalogCache) == nil {
		return make(map[string]V)
	}
	return get(catalogCache)
}

func knownMetadataKind(kind catalog.MetadataKind) bool {
	switch kind {
	case catalog.MetadataSchemas, catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll,
		catalog.MetadataPrimaryKeys, catalog.MetadataForeignKeys, catalog.MetadataViews, catalog.MetadataProcedures,
		catalog.MetadataGenerators, catalog.MetadataDomains, catalog.MetadataFunctions, catalog.MetadataIndexes, catalog.MetadataTriggers:
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

func cloneColumnSlices(source map[string][]*catalog.ColumnDesc) map[string][]*catalog.ColumnDesc {
	result := make(map[string][]*catalog.ColumnDesc, len(source))
	for key, columns := range source {
		result[key] = append([]*catalog.ColumnDesc(nil), columns...)
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

func cloneForeignKeyMap(source map[string]map[string][]*catalog.ForeignKey) map[string]map[string][]*catalog.ForeignKey {
	result := make(map[string]map[string][]*catalog.ForeignKey, len(source))
	for table, references := range source {
		copied := make(map[string][]*catalog.ForeignKey, len(references))
		for refTable, keys := range references {
			copied[refTable] = append([]*catalog.ForeignKey(nil), keys...)
		}
		result[table] = copied
	}
	return result
}

func cloneCatalogHeader(source *catalog.CatalogCache) *catalog.CatalogCache {
	if source == nil {
		return &catalog.CatalogCache{}
	}
	copy := *source
	return &copy
}

func columnsWithPrimaryKeys(columns map[string][]*catalog.ColumnDesc, primaryKeys map[string]map[string]struct{}) map[string][]*catalog.ColumnDesc {
	result := make(map[string][]*catalog.ColumnDesc, len(columns))
	for table, descriptors := range columns {
		keys := primaryKeys[table]
		copied := make([]*catalog.ColumnDesc, len(descriptors))
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

func indexGroups(indexes map[string]*catalog.IndexDesc) map[string][]*catalog.IndexDesc {
	groups := make(map[string][]*catalog.IndexDesc)
	for _, index := range indexes {
		if index == nil || strings.TrimSpace(index.RelationName) == "" {
			continue
		}
		key := catalog.CatalogCacheKey(index.RelationName)
		groups[key] = append(groups[key], index)
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].Name < group[j].Name })
	}
	return groups
}

func triggerGroups(triggers map[string]*catalog.TriggerDesc) map[string][]*catalog.TriggerDesc {
	groups := make(map[string][]*catalog.TriggerDesc)
	for _, trigger := range triggers {
		if trigger == nil || !trigger.RelationName.Valid || strings.TrimSpace(trigger.RelationName.String) == "" {
			continue
		}
		key := catalog.CatalogCacheKey(trigger.RelationName.String)
		groups[key] = append(groups[key], trigger)
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].Name < group[j].Name })
	}
	return groups
}
