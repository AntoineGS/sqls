package database

import (
	"context"
	"database/sql"
)

// MockCatalogDBRepository is a DBRepository that also implements the optional
// catalog capabilities. It is deliberately a separate type from
// MockDBRepository: if MockDBRepository itself satisfied these interfaces,
// every existing test that builds one would start taking the capability branch
// in production code and panic on a nil func field.
//
// An unset Mock… field answers with an empty result and a nil error, so a test
// stubs only the capability it exercises.
type MockCatalogDBRepository struct {
	*MockDBRepository

	MockDescribeViews      func(context.Context) ([]*ViewDesc, error)
	MockDescribeProcedures func(context.Context) ([]*ProcedureDesc, error)
	MockDescribeGenerators func(context.Context) ([]*GeneratorDesc, error)
	MockDescribeTriggers   func(context.Context) ([]*TriggerDesc, error)
	MockDescribeDomains    func(context.Context) ([]*DomainDesc, error)
	MockDescribeIndexes    func(context.Context) ([]*IndexDesc, error)
	MockDescribeFunctions  func(context.Context) ([]*FunctionDesc, error)
	MockObjectDDL          func(context.Context, ObjectKind, string) (string, error)
	MockExplainPlan        func(context.Context, string) (string, error)
}

var (
	_ DBRepository      = (*MockCatalogDBRepository)(nil)
	_ CatalogRepository = (*MockCatalogDBRepository)(nil)
	_ DDLRepository     = (*MockCatalogDBRepository)(nil)
	_ ExplainRepository = (*MockCatalogDBRepository)(nil)
)

// NewMockCatalogDBRepository returns a capability mock backed by the same
// dummy data as NewMockDBRepository, with every capability unstubbed.
func NewMockCatalogDBRepository(db *sql.DB) *MockCatalogDBRepository {
	return &MockCatalogDBRepository{
		MockDBRepository: NewMockDBRepository(db).(*MockDBRepository),
	}
}

func (m *MockCatalogDBRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error) {
	if m.MockDescribeViews == nil {
		return nil, nil
	}
	return m.MockDescribeViews(ctx)
}

func (m *MockCatalogDBRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error) {
	if m.MockDescribeProcedures == nil {
		return nil, nil
	}
	return m.MockDescribeProcedures(ctx)
}

func (m *MockCatalogDBRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error) {
	if m.MockDescribeGenerators == nil {
		return nil, nil
	}
	return m.MockDescribeGenerators(ctx)
}

func (m *MockCatalogDBRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error) {
	if m.MockDescribeTriggers == nil {
		return nil, nil
	}
	return m.MockDescribeTriggers(ctx)
}

func (m *MockCatalogDBRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error) {
	if m.MockDescribeDomains == nil {
		return nil, nil
	}
	return m.MockDescribeDomains(ctx)
}

func (m *MockCatalogDBRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error) {
	if m.MockDescribeIndexes == nil {
		return nil, nil
	}
	return m.MockDescribeIndexes(ctx)
}

func (m *MockCatalogDBRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) {
	if m.MockDescribeFunctions == nil {
		return nil, nil
	}
	return m.MockDescribeFunctions(ctx)
}

func (m *MockCatalogDBRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	if m.MockObjectDDL == nil {
		return "", nil
	}
	return m.MockObjectDDL(ctx, kind, name)
}

func (m *MockCatalogDBRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	if m.MockExplainPlan == nil {
		return "", nil
	}
	return m.MockExplainPlan(ctx, query)
}
