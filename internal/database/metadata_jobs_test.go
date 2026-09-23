package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestMetadataGenericPlanIsSerialAndIndependent(t *testing.T) {
	plan := metadataPlanFor(catalogTestRepository())
	if plan.Parallelism != 1 {
		t.Fatalf("generic parallelism = %d, want 1", plan.Parallelism)
	}
	jobs := make(map[MetadataKind]MetadataJob, len(plan.Jobs))
	for _, job := range plan.Jobs {
		jobs[job.Kind] = job
	}
	if len(jobs) != len(plan.Jobs) {
		t.Fatal("generic plan contains duplicate jobs")
	}
	for _, kind := range []MetadataKind{MetadataRelations, MetadataColumnsAll, MetadataViews, MetadataProcedures, MetadataGenerators, MetadataDomains, MetadataFunctions, MetadataIndexes, MetadataTriggers} {
		if len(jobs[kind].DependsOn) != 0 {
			t.Errorf("%s dependencies = %v, want none", kind, jobs[kind].DependsOn)
		}
	}
	for _, kind := range []MetadataKind{MetadataColumnsCurrent, MetadataForeignKeys} {
		if len(jobs[kind].DependsOn) != 1 || jobs[kind].DependsOn[0] != MetadataSchemas {
			t.Errorf("%s dependencies = %v, want schemas", kind, jobs[kind].DependsOn)
		}
	}
}

func TestMetadataGenericPlanPrefersOptionalDriverPlan(t *testing.T) {
	want := MetadataPlan{Parallelism: 2, Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{}}}}, nil
	}}}}
	repository := metadataRepo(want)
	got := metadataPlanFor(repository)
	if got.Parallelism != want.Parallelism || len(got.Jobs) != 1 || got.Jobs[0].Kind != MetadataViews {
		t.Fatalf("optional plan = %#v, want driver plan %#v", got, want)
	}
}

func TestMetadataGenericMocksDoNotGainOptionalCapabilities(t *testing.T) {
	var repository any = NewMockDBRepository(nil)
	if _, ok := repository.(MetadataPlanRepository); ok {
		t.Fatal("plain MockDBRepository unexpectedly implements MetadataPlanRepository")
	}
	if _, ok := repository.(CatalogRepository); ok {
		t.Fatal("plain MockDBRepository unexpectedly implements CatalogRepository")
	}
}

func TestMetadataGenericCurrentSchemaFailureIsIsolated(t *testing.T) {
	repository := catalogTestRepository()
	repository.MockDatabase = func(context.Context) (string, error) { return "", errors.New("current schema unavailable") }
	loader := NewMetadataLoader()
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, repository)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata load did not settle")
	}
	snapshot := loader.Snapshot()
	if !snapshot.Settled() {
		t.Fatalf("snapshot did not settle: %#v", snapshot.Status)
	}
	for _, kind := range []MetadataKind{MetadataSchemas, MetadataColumnsCurrent, MetadataForeignKeys} {
		if got := snapshot.Status[kind].State; got != MetadataFailed && got != MetadataBlocked {
			t.Errorf("%s state = %s, want failed or blocked", kind, got)
		}
	}
	for _, kind := range []MetadataKind{MetadataColumnsAll, MetadataViews, MetadataProcedures, MetadataGenerators, MetadataDomains, MetadataFunctions, MetadataIndexes, MetadataTriggers, MetadataRelations} {
		if got := snapshot.Status[kind].State; got != MetadataReady {
			t.Errorf("independent %s state = %s, want ready", kind, got)
		}
	}
	if _, ok := snapshot.Cache.View("customer_view"); !ok {
		t.Error("view sibling did not publish")
	}
	if _, ok := snapshot.Cache.Procedure("drop_customer"); !ok {
		t.Error("procedure sibling did not publish")
	}
	for _, kind := range []MetadataKind{MetadataGenerators, MetadataDomains, MetadataFunctions, MetadataIndexes, MetadataTriggers} {
		if !snapshot.Cache.MetadataReady(kind) {
			t.Errorf("independent %s payload was not marked ready", kind)
		}
	}
	if _, ok := snapshot.Cache.Generator("gen_customer_id"); !ok {
		t.Error("generator sibling did not publish")
	}
	if _, ok := snapshot.Cache.Domain("email_address"); !ok {
		t.Error("domain sibling did not publish")
	}
	if names := snapshot.Cache.SortedFunctions(); len(names) != 2 {
		t.Errorf("function sibling names = %v, want 2 functions", names)
	}
	if _, ok := snapshot.Cache.Index("idx_customer_pk"); !ok || len(snapshot.Cache.IndexesForTable("customer")) != 2 {
		t.Error("index sibling or table grouping did not publish")
	}
	if _, ok := snapshot.Cache.Trigger("customer_bi"); !ok || len(snapshot.Cache.TriggersForTable("customer")) != 1 {
		t.Error("trigger sibling or table grouping did not publish")
	}
}

func TestMetadataGenericFragmentsOwnRepositoryDescriptors(t *testing.T) {
	repository := catalogTestRepository()
	column := &ColumnDesc{ColumnBase: ColumnBase{Schema: "world", Table: "customer", Name: "original"}, Type: "INTEGER"}
	repository.MockDescribeDatabaseTable = func(context.Context) ([]*ColumnDesc, error) { return []*ColumnDesc{column}, nil }
	repository.MockDescribeDatabaseTableBySchema = func(context.Context, string) ([]*ColumnDesc, error) { return []*ColumnDesc{column}, nil }

	left := &ColumnBase{Schema: "world", Table: "child", Name: "child_id"}
	right := &ColumnBase{Schema: "world", Table: "parent", Name: "id"}
	fk := &ForeignKey{{left, right}}
	repository.MockDescribeForeignKeysBySchema = func(context.Context, string) ([]*ForeignKey, error) { return []*ForeignKey{fk}, nil }

	viewColumn := &ColumnDesc{ColumnBase: ColumnBase{Name: "view_column"}, Type: "VARCHAR"}
	view := &ViewDesc{Name: "owned_view", Columns: []*ColumnDesc{viewColumn}}
	parameter := &ProcedureParameterDesc{Name: "input_value", Position: 1}
	procedure := &ProcedureDesc{Name: "owned_procedure", InputParameters: []*ProcedureParameterDesc{parameter}}
	generator := &GeneratorDesc{Name: "owned_generator"}
	domain := &DomainDesc{Name: "owned_domain", Type: "INTEGER"}
	argument := &FunctionArgumentDesc{Name: "argument", Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "INTEGER"}
	function := &FunctionDesc{Name: "owned_function", ReturnType: "INTEGER", Arguments: []*FunctionArgumentDesc{argument}}
	index := &IndexDesc{Name: "owned_index", RelationName: "customer", Columns: []string{"original_column"}}
	trigger := &TriggerDesc{Name: "owned_trigger", RelationName: sql.NullString{String: "customer", Valid: true}, Event: "BEFORE INSERT"}
	repository.MockDescribeViews = func(context.Context) ([]*ViewDesc, error) { return []*ViewDesc{view}, nil }
	repository.MockDescribeProcedures = func(context.Context) ([]*ProcedureDesc, error) { return []*ProcedureDesc{procedure}, nil }
	repository.MockDescribeGenerators = func(context.Context) ([]*GeneratorDesc, error) { return []*GeneratorDesc{generator}, nil }
	repository.MockDescribeDomains = func(context.Context) ([]*DomainDesc, error) { return []*DomainDesc{domain}, nil }
	repository.MockDescribeFunctions = func(context.Context) ([]*FunctionDesc, error) { return []*FunctionDesc{function}, nil }
	repository.MockDescribeIndexes = func(context.Context) ([]*IndexDesc, error) { return []*IndexDesc{index}, nil }
	repository.MockDescribeTriggers = func(context.Context) ([]*TriggerDesc, error) { return []*TriggerDesc{trigger}, nil }

	loader := NewMetadataLoader()
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, repository)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata load did not settle")
	}
	snapshot := loader.Snapshot()

	// Mutate source-owned objects after publication. Neither descriptors nor
	// mutable nested slices/pointers may be shared with the retained snapshot.
	column.Name = "mutated"
	left.Table, right.Name = "mutated_table", "mutated_id"
	(*fk)[0][0] = &ColumnBase{Table: "replacement"}
	viewColumn.Name = "mutated_view_column"
	parameter.Name = "mutated_parameter"
	procedure.InputParameters[0] = &ProcedureParameterDesc{Name: "replacement_parameter"}
	generator.Name = "mutated_generator"
	domain.Type = "mutated_type"
	argument.Name = "mutated_argument"
	function.Arguments[0] = &FunctionArgumentDesc{Name: "replacement_argument"}
	index.Columns[0] = "mutated_column"
	trigger.Event = "mutated_event"

	if got := snapshot.Cache.ColumnsWithParent[columnDatabaseKey("world", "customer")][0].Name; got != "original" {
		t.Errorf("published column name = %q, want original", got)
	}
	publishedFK := snapshot.Cache.ForeignKeys["child"]["parent"][0]
	if got := (*publishedFK)[0][0].Table; got != "child" {
		t.Errorf("published foreign key table = %q, want child", got)
	}
	if got := snapshot.Cache.Catalog.Views["OWNED_VIEW"].Columns[0].Name; got != "view_column" {
		t.Errorf("published view column = %q, want view_column", got)
	}
	if got := snapshot.Cache.Catalog.Procedures["OWNED_PROCEDURE"].InputParameters[0].Name; got != "input_value" {
		t.Errorf("published procedure parameter = %q, want input_value", got)
	}
	if got := snapshot.Cache.Catalog.Generators["OWNED_GENERATOR"].Name; got != "owned_generator" {
		t.Errorf("published generator name = %q, want owned_generator", got)
	}
	if got := snapshot.Cache.Catalog.Domains["OWNED_DOMAIN"].Type; got != "INTEGER" {
		t.Errorf("published domain type = %q, want INTEGER", got)
	}
	if got := snapshot.Cache.Catalog.Functions["OWNED_FUNCTION"].Arguments[0].Name; got != "argument" {
		t.Errorf("published function argument = %q, want argument", got)
	}
	if got := snapshot.Cache.Catalog.Indexes["OWNED_INDEX"].Columns[0]; got != "original_column" {
		t.Errorf("published index column = %q, want original_column", got)
	}
	if got := snapshot.Cache.Catalog.Triggers["OWNED_TRIGGER"].Event; got != "BEFORE INSERT" {
		t.Errorf("published trigger event = %q, want BEFORE INSERT", got)
	}
}

func TestMetadataGenericSchemaFallbackIsDeterministic(t *testing.T) {
	repository := NewMockDBRepository(nil).(*MockDBRepository)
	repository.MockDatabase = func(context.Context) (string, error) { return "", nil }
	repository.MockDatabases = func(context.Context) ([]string, error) { return []string{"zeta", "Alpha", "beta"}, nil }
	plan := metadataPlanFor(repository)
	var schemas MetadataJob
	for _, job := range plan.Jobs {
		if job.Kind == MetadataSchemas {
			schemas = job
		}
	}
	patch, err := schemas.Run(context.Background(), newMetadataCache())
	if err != nil {
		t.Fatalf("schemas job error = %v", err)
	}
	if got := patch.Cache.defaultSchema; got != "Alpha" {
		t.Errorf("fallback default schema = %q, want Alpha", got)
	}
}

func TestMetadataGenericForeignKeyGroupingRejectsMalformedPairs(t *testing.T) {
	for name, keys := range map[string][]*ForeignKey{
		"empty key":  {new(ForeignKey)},
		"empty pair": {&ForeignKey{{nil, nil}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := groupForeignKeys(keys); err == nil {
				t.Fatal("groupForeignKeys() error = nil, want malformed input error")
			}
		})
	}
}

func TestMetadataGenericForeignKeyGroupingValidatesEveryPair(t *testing.T) {
	left := &ColumnBase{Table: "child"}
	right := &ColumnBase{Table: "parent"}
	malformedLaterPair := &ForeignKey{{left, right}, {left, nil}}
	if _, err := groupForeignKeys([]*ForeignKey{malformedLaterPair}); err == nil {
		t.Fatal("groupForeignKeys() error = nil for nil endpoint in a later pair")
	}

	repository := catalogTestRepository()
	repository.MockDescribeForeignKeysBySchema = func(context.Context, string) ([]*ForeignKey, error) {
		return []*ForeignKey{malformedLaterPair}, nil
	}
	var foreignKeys MetadataJob
	for _, job := range metadataPlanFor(repository).Jobs {
		if job.Kind == MetadataForeignKeys {
			foreignKeys = job
			break
		}
	}
	if _, err := foreignKeys.Run(context.Background(), &DBCache{}); err == nil {
		t.Fatal("foreign-key job accepted malformed later pair")
	}
}
