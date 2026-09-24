package database

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/dialect"
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

func TestCacheBuildPrimaryMarksSuccessfulCategoriesReady(t *testing.T) {
	repository := NewMockDBRepository(nil)
	cache, err := NewDBCacheUpdater(repository).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	for _, kind := range []MetadataKind{MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataForeignKeys} {
		if !cache.MetadataReady(kind) {
			t.Errorf("successful primary generation did not mark %s ready", kind)
		}
	}
}

func catalogTestRepository() *MockCapabilityRepository {
	repository := NewMockCapabilityRepository()
	repository.MockDescribeViews = func(context.Context) ([]*ViewDesc, error) {
		return []*ViewDesc{{Name: "Customer_View", ViewSource: sql.NullString{String: "SELECT 1", Valid: true}}}, nil
	}
	repository.MockDescribeProcedures = func(context.Context) ([]*ProcedureDesc, error) {
		return []*ProcedureDesc{
			{Name: "Add_Customer", Source: sql.NullString{String: "BEGIN END", Valid: true}},
			{Name: "Drop_Customer"},
		}, nil
	}
	repository.MockDescribeGenerators = func(context.Context) ([]*GeneratorDesc, error) {
		return []*GeneratorDesc{{Name: "Gen_Customer_Id", ID: sql.NullInt64{Int64: 1, Valid: true}}}, nil
	}
	repository.MockDescribeDomains = func(context.Context) ([]*DomainDesc, error) {
		return []*DomainDesc{{Name: "Email_Address", Type: "VARCHAR(100)"}}, nil
	}
	repository.MockDescribeFunctions = func(context.Context) ([]*FunctionDesc, error) {
		return []*FunctionDesc{{Name: "F_Ltrim"}, {Name: "F_Rtrim"}}, nil
	}
	repository.MockDescribeIndexes = func(context.Context) ([]*IndexDesc, error) {
		return []*IndexDesc{
			{Name: "Idx_Customer_Pk", RelationName: "Customer", Columns: []string{"ID"}},
			{Name: "Idx_Customer_Code", RelationName: "Customer", Columns: []string{"CODE"}},
			{Name: "Idx_Parent_Pk", RelationName: "Parent", Columns: []string{"PARENT_B"}},
		}, nil
	}
	repository.MockDescribeTriggers = func(context.Context) ([]*TriggerDesc, error) {
		return []*TriggerDesc{
			{Name: "Customer_Bi", RelationName: sql.NullString{String: "Customer", Valid: true}},
			{Name: "Db_Connect"},
		}, nil
	}
	return repository
}

func TestGenerateCatalogCacheFromCapabilityRepository(t *testing.T) {
	repository := catalogTestRepository()

	catalog, ok, err := NewDBCacheUpdater(repository).GenerateCatalogCache(context.Background())
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	if !ok {
		t.Fatal("GenerateCatalogCache() reported no extended catalog for a CatalogRepository")
	}
	if catalog == nil {
		t.Fatal("GenerateCatalogCache() returned a nil catalog with ok == true")
	}

	// Keys are upper-cased, because InterBase stores catalog names upper-cased
	// while users type them lower-cased.
	for _, key := range []string{"CUSTOMER_VIEW", "ADD_CUSTOMER", "GEN_CUSTOMER_ID", "EMAIL_ADDRESS", "F_LTRIM", "IDX_CUSTOMER_PK", "CUSTOMER_BI"} {
		found := false
		for _, keys := range []map[string]bool{
			catalogKeySet(catalog.Views), catalogKeySet(catalog.Procedures), catalogKeySet(catalog.Generators),
			catalogKeySet(catalog.Domains), catalogKeySet(catalog.Functions), catalogKeySet(catalog.Indexes),
			catalogKeySet(catalog.Triggers),
		} {
			if keys[key] {
				found = true
			}
		}
		if !found {
			t.Errorf("no cache map is keyed by %q", key)
		}
	}

	if got, want := len(catalog.IndexesByTable["CUSTOMER"]), 2; got != want {
		t.Errorf("IndexesByTable[CUSTOMER] = %d indexes, want %d", got, want)
	}
	if got, want := len(catalog.TriggersByTable["CUSTOMER"]), 1; got != want {
		t.Errorf("TriggersByTable[CUSTOMER] = %d triggers, want %d", got, want)
	}
	// A database-level trigger has no relation and must not be grouped under
	// the empty table name.
	if got := len(catalog.TriggersByTable[""]); got != 0 {
		t.Errorf("TriggersByTable[\"\"] = %d triggers, want 0", got)
	}

	cache := &DBCache{Catalog: catalog}
	if !cache.HasCatalog() {
		t.Error("HasCatalog() = false, want true")
	}

	// Every singular accessor normalises the name it is given: callers pass
	// the identifier text as the user typed it and never upper-case at the
	// call site.
	if view, ok := cache.View("customer_view"); !ok || view.Name != "Customer_View" {
		t.Errorf("View(lower case) = (%#v, %v), want the cached view", view, ok)
	}
	if procedure, ok := cache.Procedure("Add_Customer"); !ok || !procedure.Source.Valid {
		t.Errorf("Procedure() = (%#v, %v), want the cached procedure", procedure, ok)
	}
	if generator, ok := cache.Generator("gen_customer_id"); !ok || generator.ID.Int64 != 1 {
		t.Errorf("Generator(lower case) = (%#v, %v), want the cached generator", generator, ok)
	}
	if domain, ok := cache.Domain("email_address"); !ok || domain.Type != "VARCHAR(100)" {
		t.Errorf("Domain(lower case) = (%#v, %v), want the cached domain", domain, ok)
	}
	if function, ok := cache.Function("f_ltrim"); !ok || function.Name != "F_Ltrim" {
		t.Errorf("Function(lower case) = (%#v, %v), want the cached function", function, ok)
	}
	if index, ok := cache.Index("idx_parent_pk"); !ok || index.RelationName != "Parent" {
		t.Errorf("Index(lower case) = (%#v, %v), want the cached index", index, ok)
	}
	if trigger, ok := cache.Trigger("customer_bi"); !ok || trigger.Name != "Customer_Bi" {
		t.Errorf("Trigger(lower case) = (%#v, %v), want the cached trigger", trigger, ok)
	}
	if _, ok := cache.View("no_such_view"); ok {
		t.Error("View(unknown) reported ok == true")
	}

	// The grouped accessors normalise their table argument the same way.
	if got := len(cache.IndexesForTable("customer")); got != 2 {
		t.Errorf("IndexesForTable(lower case) = %d indexes, want 2", got)
	}
	if got := len(cache.TriggersForTable("customer")); got != 1 {
		t.Errorf("TriggersForTable(lower case) = %d triggers, want 1", got)
	}
	if got := len(cache.IndexesForTable("no_such_table")); got != 0 {
		t.Errorf("IndexesForTable(unknown) = %d indexes, want 0", got)
	}

	if got, want := cache.SortedProcedures(), []string{"Add_Customer", "Drop_Customer"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedProcedures() = %v, want %v", got, want)
	}
	if got, want := cache.SortedViews(), []string{"Customer_View"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedViews() = %v, want %v", got, want)
	}
	if got, want := cache.SortedGenerators(), []string{"Gen_Customer_Id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedGenerators() = %v, want %v", got, want)
	}
	if got, want := cache.SortedFunctions(), []string{"F_Ltrim", "F_Rtrim"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedFunctions() = %v, want %v", got, want)
	}
}

func catalogKeySet[V any](source map[string]V) map[string]bool {
	keys := make(map[string]bool, len(source))
	for key := range source {
		keys[key] = true
	}
	return keys
}

func TestGenerateCatalogCacheWithoutCapability(t *testing.T) {
	catalog, ok, err := NewDBCacheUpdater(NewMockDBRepository(nil)).GenerateCatalogCache(context.Background())
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	if ok || catalog != nil {
		t.Fatalf("GenerateCatalogCache() = (%#v, %v), want (nil, false) for a repository without the capability", catalog, ok)
	}

	// Every accessor is nil-safe: a nil *CatalogCache is the normal state for
	// every driver but InterBase, and for InterBase before the first
	// successful secondary pass.
	for _, cache := range []*DBCache{{}, {Catalog: nil}, nil} {
		if cache.HasCatalog() {
			t.Error("HasCatalog() = true without a catalog")
		}
		if _, ok := cache.View("anything"); ok {
			t.Error("View() reported ok == true without a catalog")
		}
		if _, ok := cache.Procedure("anything"); ok {
			t.Error("Procedure() reported ok == true without a catalog")
		}
		if _, ok := cache.Generator("anything"); ok {
			t.Error("Generator() reported ok == true without a catalog")
		}
		if _, ok := cache.Domain("anything"); ok {
			t.Error("Domain() reported ok == true without a catalog")
		}
		if _, ok := cache.Function("anything"); ok {
			t.Error("Function() reported ok == true without a catalog")
		}
		if _, ok := cache.Index("anything"); ok {
			t.Error("Index() reported ok == true without a catalog")
		}
		if _, ok := cache.Trigger("anything"); ok {
			t.Error("Trigger() reported ok == true without a catalog")
		}
		if len(cache.IndexesForTable("anything")) != 0 || len(cache.TriggersForTable("anything")) != 0 {
			t.Error("a grouped accessor returned rows without a catalog")
		}
		if len(cache.SortedProcedures()) != 0 || len(cache.SortedViews()) != 0 ||
			len(cache.SortedGenerators()) != 0 || len(cache.SortedFunctions()) != 0 {
			t.Error("a sorted accessor returned names without a catalog")
		}
	}
}

func TestMockCapabilityRepositoryIsDistinctFromMockDBRepository(t *testing.T) {
	// If MockDBRepository itself satisfied the capability interfaces, every
	// existing handler test would start taking the capability branch and panic
	// on a nil func field.
	plain := DBRepository(NewMockDBRepository(nil))
	for name, ok := range map[string]bool{
		"CatalogRepository": func() bool { _, ok := plain.(CatalogRepository); return ok }(),
		"DDLRepository":     func() bool { _, ok := plain.(DDLRepository); return ok }(),
		"ExplainRepository": func() bool { _, ok := plain.(ExplainRepository); return ok }(),
	} {
		if ok {
			t.Errorf("MockDBRepository must not implement %s", name)
		}
	}

	capable := DBRepository(NewMockCapabilityRepository())
	for name, ok := range map[string]bool{
		"CatalogRepository": func() bool { _, ok := capable.(CatalogRepository); return ok }(),
		"DDLRepository":     func() bool { _, ok := capable.(DDLRepository); return ok }(),
		"ExplainRepository": func() bool { _, ok := capable.(ExplainRepository); return ok }(),
	} {
		if !ok {
			t.Errorf("MockCapabilityRepository must implement %s", name)
		}
	}
	if got, want := capable.Driver(), dialect.DatabaseDriver("mock"); got != want {
		t.Errorf("mock driver = %q, want %q", got, want)
	}

	// Unset func fields answer empty rather than panicking, so a test that
	// exercises one capability need not stub all of them.
	bare := NewMockCapabilityRepository()
	views, err := bare.DescribeViews(context.Background())
	if err != nil || len(views) != 0 {
		t.Errorf("DescribeViews() on an unstubbed mock = (%v, %v), want (empty, nil)", views, err)
	}
	if ddl, err := bare.ObjectDDL(context.Background(), ObjectKindTable, "T"); !errors.Is(err, ErrObjectNotFound) || ddl != "" {
		t.Errorf("ObjectDDL() on an unstubbed mock = (%q, %v), want (\"\", ErrObjectNotFound)", ddl, err)
	}
}
