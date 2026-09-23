package database

import (
	"context"
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
