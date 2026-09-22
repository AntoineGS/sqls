package database

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// countingSnapshotRepository records how a cache build reached it: which
// repository served each read, and how many snapshots were opened and closed.
type countingSnapshotRepository struct {
	*MockDBRepository

	snapshotSupported bool
	// served is the shared tally; a snapshot shares its source's pointer so
	// the test can see which object answered.
	served *[]string
	opened *int
	closed *int
	// isSnapshot marks a repository handed out by CatalogSnapshot.
	isSnapshot bool
}

func newCountingSnapshotRepository(snapshotSupported bool) *countingSnapshotRepository {
	served := make([]string, 0)
	opened := 0
	closed := 0
	repository := &countingSnapshotRepository{
		MockDBRepository:  NewMockDBRepository(nil).(*MockDBRepository),
		snapshotSupported: snapshotSupported,
		served:            &served,
		opened:            &opened,
		closed:            &closed,
	}
	return repository
}

func (r *countingSnapshotRepository) record(method string) {
	source := "direct"
	if r.isSnapshot {
		source = "snapshot"
	}
	*r.served = append(*r.served, source+":"+method)
}

func (r *countingSnapshotRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	r.record("SchemaTables")
	return r.MockDBRepository.SchemaTables(ctx)
}

func (r *countingSnapshotRepository) DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error) {
	r.record("DescribeDatabaseTableBySchema")
	return r.MockDBRepository.DescribeDatabaseTableBySchema(ctx, schemaName)
}

func (r *countingSnapshotRepository) DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error) {
	r.record("DescribeForeignKeysBySchema")
	return r.MockDBRepository.DescribeForeignKeysBySchema(ctx, schemaName)
}

func (r *countingSnapshotRepository) CatalogSnapshot(ctx context.Context) (DBRepository, func() error, error) {
	if !r.snapshotSupported {
		return nil, nil, errors.New("this fake does not support snapshots")
	}
	*r.opened++
	bound := &countingSnapshotRepository{
		MockDBRepository:  r.MockDBRepository,
		snapshotSupported: true,
		served:            r.served,
		opened:            r.opened,
		closed:            r.closed,
		isSnapshot:        true,
	}
	return bound, func() error { *r.closed++; return nil }, nil
}

// plainRepository is the same fake without the capability, so the direct path
// is exercised by a type that cannot accidentally satisfy the interface.
type plainRepository struct {
	*MockDBRepository
	served *[]string
}

func (r *plainRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	*r.served = append(*r.served, "direct:SchemaTables")
	return r.MockDBRepository.SchemaTables(ctx)
}

func TestCacheBuildUsesOneCatalogSnapshot(t *testing.T) {
	repository := newCountingSnapshotRepository(true)
	if _, ok := DBRepository(repository).(CatalogSnapshotRepository); !ok {
		t.Fatal("the fake must implement CatalogSnapshotRepository")
	}

	if _, err := NewDBCacheUpdater(repository).GenerateDBCachePrimary(context.Background()); err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}

	if *repository.opened != 1 {
		t.Errorf("CatalogSnapshot was opened %d times, want exactly 1 per cache build", *repository.opened)
	}
	if *repository.closed != 1 {
		t.Errorf("the snapshot was closed %d times, want exactly 1", *repository.closed)
	}
	want := []string{
		"snapshot:SchemaTables",
		"snapshot:DescribeDatabaseTableBySchema",
		"snapshot:DescribeForeignKeysBySchema",
	}
	if !reflect.DeepEqual(*repository.served, want) {
		t.Errorf("reads served = %v, want every primary-pass read served from the snapshot %v", *repository.served, want)
	}
}

func TestCacheBuildWithoutSnapshotCapabilityUsesTheRepositoryDirectly(t *testing.T) {
	served := make([]string, 0)
	repository := &plainRepository{MockDBRepository: NewMockDBRepository(nil).(*MockDBRepository), served: &served}
	if _, ok := DBRepository(repository).(CatalogSnapshotRepository); ok {
		t.Fatal("the plain fake must not implement CatalogSnapshotRepository")
	}

	cache, err := NewDBCacheUpdater(repository).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	if cache == nil || len(cache.SchemaTables) == 0 {
		t.Fatal("the direct path must still build a usable cache")
	}
	if len(served) != 1 || served[0] != "direct:SchemaTables" {
		t.Errorf("reads served = %v, want the repository itself to answer", served)
	}
}
