package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// metadataPlanFor prefers a repository's explicit driver plan. Generic
// repositories are adapted from their existing calls and kept serial until
// driver-specific concurrency has been verified.
func metadataPlanFor(repo DBRepository) MetadataPlan {
	if planner, ok := repo.(MetadataPlanRepository); ok {
		return planner.MetadataPlan()
	}

	plan := MetadataPlan{Parallelism: 1}
	plan.Jobs = append(plan.Jobs,
		MetadataJob{Kind: MetadataSchemas, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			current, err := repo.CurrentSchema(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			schemas, err := repo.Schemas(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			fragment := &DBCache{Schemas: make(map[string]string, len(schemas))}
			for _, schema := range schemas {
				fragment.Schemas[strings.ToUpper(schema)] = schema
			}
			if current == "" {
				sorted := append([]string(nil), schemas...)
				sort.Strings(sorted)
				if len(sorted) > 0 {
					current = sorted[0]
				}
			}
			fragment.defaultSchema = current
			return MetadataPatch{Cache: fragment, Count: len(fragment.Schemas)}, nil
		}},
		MetadataJob{Kind: MetadataRelations, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			relations, err := repo.SchemaTables(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			fragment := &DBCache{SchemaTables: make(map[string][]string, len(relations))}
			count := 0
			for schema, tables := range relations {
				fragment.SchemaTables[strings.ToUpper(schema)] = append([]string(nil), tables...)
				count += len(tables)
			}
			return MetadataPatch{Cache: fragment, Count: count}, nil
		}},
		MetadataJob{Kind: MetadataColumnsCurrent, DependsOn: []MetadataKind{MetadataSchemas}, Run: func(ctx context.Context, cache *DBCache) (MetadataPatch, error) {
			columns, err := repo.DescribeDatabaseTableBySchema(ctx, cache.defaultSchema)
			if err != nil {
				return MetadataPatch{}, err
			}
			return MetadataPatch{Cache: &DBCache{ColumnsWithParent: genColumnMap(cloneColumnDescs(columns))}, Count: len(columns)}, nil
		}},
		MetadataJob{Kind: MetadataColumnsAll, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			columns, err := repo.DescribeDatabaseTable(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			return MetadataPatch{Cache: &DBCache{ColumnsWithParent: genColumnMap(cloneColumnDescs(columns))}, Count: len(columns)}, nil
		}},
		MetadataJob{Kind: MetadataForeignKeys, DependsOn: []MetadataKind{MetadataSchemas}, Run: func(ctx context.Context, cache *DBCache) (MetadataPatch, error) {
			keys, err := repo.DescribeForeignKeysBySchema(ctx, cache.defaultSchema)
			if err != nil {
				return MetadataPatch{}, err
			}
			grouped, err := groupForeignKeys(cloneForeignKeys(keys))
			if err != nil {
				return MetadataPatch{}, err
			}
			return MetadataPatch{Cache: &DBCache{ForeignKeys: grouped}, Count: len(keys)}, nil
		}},
	)
	if catalog, ok := repo.(CatalogRepository); ok {
		plan.Jobs = append(plan.Jobs, catalogMetadataJobs(catalog)...)
	}
	return plan
}

func catalogMetadataJobs(repo CatalogRepository) []MetadataJob {
	return []MetadataJob{
		{Kind: MetadataViews, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeViews(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Views: make(map[string]*ViewDesc, len(values))}
			for _, value := range values {
				if value != nil {
					cache.Views[catalogCacheKey(value.Name)] = cloneViewDesc(value)
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataProcedures, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeProcedures(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Procedures: make(map[string]*ProcedureDesc, len(values))}
			for _, value := range values {
				if value != nil {
					cache.Procedures[catalogCacheKey(value.Name)] = cloneProcedureDesc(value)
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataGenerators, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeGenerators(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Generators: make(map[string]*GeneratorDesc, len(values))}
			for _, value := range values {
				if value != nil {
					copy := *value
					cache.Generators[catalogCacheKey(value.Name)] = &copy
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataDomains, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeDomains(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Domains: make(map[string]*DomainDesc, len(values))}
			for _, value := range values {
				if value != nil {
					copy := *value
					cache.Domains[catalogCacheKey(value.Name)] = &copy
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataFunctions, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeFunctions(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Functions: make(map[string]*FunctionDesc, len(values))}
			for _, value := range values {
				if value != nil {
					cache.Functions[catalogCacheKey(value.Name)] = cloneFunctionDesc(value)
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataIndexes, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeIndexes(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Indexes: make(map[string]*IndexDesc, len(values))}
			for _, value := range values {
				if value != nil {
					cache.Indexes[catalogCacheKey(value.Name)] = cloneIndexDesc(value)
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
		{Kind: MetadataTriggers, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			values, err := repo.DescribeTriggers(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			cache := &CatalogCache{Triggers: make(map[string]*TriggerDesc, len(values))}
			for _, value := range values {
				if value != nil {
					copy := *value
					cache.Triggers[catalogCacheKey(value.Name)] = &copy
				}
			}
			return MetadataPatch{Cache: &DBCache{Catalog: cache}, Count: len(values)}, nil
		}},
	}
}

func cloneColumnDescs(columns []*ColumnDesc) []*ColumnDesc {
	cloned := make([]*ColumnDesc, len(columns))
	for i, column := range columns {
		if column != nil {
			copy := *column
			cloned[i] = &copy
		}
	}
	return cloned
}

func cloneForeignKeys(keys []*ForeignKey) []*ForeignKey {
	cloned := make([]*ForeignKey, len(keys))
	for i, key := range keys {
		if key == nil {
			continue
		}
		copy := make(ForeignKey, len(*key))
		for pairIndex, pair := range *key {
			for columnIndex, column := range pair {
				if column != nil {
					columnCopy := *column
					copy[pairIndex][columnIndex] = &columnCopy
				}
			}
		}
		cloned[i] = &copy
	}
	return cloned
}

func cloneViewDesc(view *ViewDesc) *ViewDesc {
	copy := *view
	copy.Columns = cloneColumnDescs(view.Columns)
	return &copy
}

func cloneProcedureDesc(procedure *ProcedureDesc) *ProcedureDesc {
	copy := *procedure
	copy.InputParameters = cloneProcedureParameters(procedure.InputParameters)
	copy.OutputParameters = cloneProcedureParameters(procedure.OutputParameters)
	return &copy
}

func cloneProcedureParameters(parameters []*ProcedureParameterDesc) []*ProcedureParameterDesc {
	cloned := make([]*ProcedureParameterDesc, len(parameters))
	for i, parameter := range parameters {
		if parameter != nil {
			copy := *parameter
			cloned[i] = &copy
		}
	}
	return cloned
}

func cloneFunctionDesc(function *FunctionDesc) *FunctionDesc {
	copy := *function
	copy.Arguments = make([]*FunctionArgumentDesc, len(function.Arguments))
	for i, argument := range function.Arguments {
		if argument != nil {
			argumentCopy := *argument
			copy.Arguments[i] = &argumentCopy
		}
	}
	return &copy
}

func cloneIndexDesc(index *IndexDesc) *IndexDesc {
	copy := *index
	copy.Columns = append([]string(nil), index.Columns...)
	return &copy
}

func groupForeignKeys(keys []*ForeignKey) (map[string]map[string][]*ForeignKey, error) {
	grouped := make(map[string]map[string][]*ForeignKey)
	for i, key := range keys {
		if err := validateForeignKey(key); err != nil {
			return nil, fmt.Errorf("malformed foreign key at index %d: %w", i, err)
		}
		pair := (*key)[0]
		left, right := pair[0].Table, pair[1].Table
		if grouped[left] == nil {
			grouped[left] = make(map[string][]*ForeignKey)
		}
		grouped[left][right] = append(grouped[left][right], key)
		if grouped[right] == nil {
			grouped[right] = make(map[string][]*ForeignKey)
		}
		grouped[right][left] = append(grouped[right][left], key)
	}
	return grouped, nil
}

func validateForeignKey(key *ForeignKey) error {
	if key == nil || len(*key) == 0 {
		return fmt.Errorf("missing table pair")
	}
	for pairIndex, pair := range *key {
		if len(pair) != 2 || pair[0] == nil || pair[1] == nil {
			return fmt.Errorf("pair %d is missing a table endpoint", pairIndex)
		}
	}
	return nil
}
