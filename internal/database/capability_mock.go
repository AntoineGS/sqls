package database

import (
	"context"
	"fmt"
	"sync"
)

// ObjectDDLCall records one ObjectDDL request made against the mock.
type ObjectDDLCall struct {
	Kind ObjectKind
	Name string
}

// MockCapabilityRepository is a DBRepository that also implements the optional
// catalog capabilities. It is deliberately a separate type from
// MockDBRepository: if MockDBRepository itself satisfied these interfaces,
// every existing test that builds one would start taking the capability branch
// in production code and panic on a nil func field.
//
// An unset Describe* field answers with an empty result and a nil error, so a
// test stubs only the capability it exercises. ObjectDDL and ExplainPlan are
// always populated by the constructor instead, and record every call under a
// mutex so a test can assert what was asked for.
type MockCapabilityRepository struct {
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

	mu               sync.Mutex
	objectDDLCalls   []ObjectDDLCall
	explainPlanCalls []string
}

var (
	_ DBRepository      = (*MockCapabilityRepository)(nil)
	_ CatalogRepository = (*MockCapabilityRepository)(nil)
	_ DDLRepository     = (*MockCapabilityRepository)(nil)
	_ ExplainRepository = (*MockCapabilityRepository)(nil)
)

// NewMockCapabilityRepository returns a capability mock backed by the same
// dummy data as NewMockDBRepository, with every Describe* capability
// unstubbed. ObjectDDL defaults to ErrObjectNotFound so a test that forgets to
// stub it sees the not-found branch rather than fabricated DDL.
func NewMockCapabilityRepository() *MockCapabilityRepository {
	return &MockCapabilityRepository{
		// NewMockDBRepository ignores its argument entirely.
		MockDBRepository: NewMockDBRepository(nil).(*MockDBRepository),
		MockObjectDDL: func(context.Context, ObjectKind, string) (string, error) {
			return "", ErrObjectNotFound
		},
		MockExplainPlan: func(context.Context, string) (string, error) {
			return "", nil
		},
	}
}

func (m *MockCapabilityRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error) {
	if m.MockDescribeViews == nil {
		return nil, nil
	}
	return m.MockDescribeViews(ctx)
}

func (m *MockCapabilityRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error) {
	if m.MockDescribeProcedures == nil {
		return nil, nil
	}
	return m.MockDescribeProcedures(ctx)
}

func (m *MockCapabilityRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error) {
	if m.MockDescribeGenerators == nil {
		return nil, nil
	}
	return m.MockDescribeGenerators(ctx)
}

func (m *MockCapabilityRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error) {
	if m.MockDescribeTriggers == nil {
		return nil, nil
	}
	return m.MockDescribeTriggers(ctx)
}

func (m *MockCapabilityRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error) {
	if m.MockDescribeDomains == nil {
		return nil, nil
	}
	return m.MockDescribeDomains(ctx)
}

func (m *MockCapabilityRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error) {
	if m.MockDescribeIndexes == nil {
		return nil, nil
	}
	return m.MockDescribeIndexes(ctx)
}

func (m *MockCapabilityRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) {
	if m.MockDescribeFunctions == nil {
		return nil, nil
	}
	return m.MockDescribeFunctions(ctx)
}

func (m *MockCapabilityRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	m.mu.Lock()
	m.objectDDLCalls = append(m.objectDDLCalls, ObjectDDLCall{Kind: kind, Name: name})
	m.mu.Unlock()
	return m.MockObjectDDL(ctx, kind, name)
}

func (m *MockCapabilityRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	m.mu.Lock()
	m.explainPlanCalls = append(m.explainPlanCalls, query)
	m.mu.Unlock()
	return m.MockExplainPlan(ctx, query)
}

func (m *MockCapabilityRepository) ObjectDDLCalls() []ObjectDDLCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ObjectDDLCall(nil), m.objectDDLCalls...)
}

func (m *MockCapabilityRepository) ExplainPlanCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.explainPlanCalls...)
}

// unsupportedDDLError carries the structured detail UnsupportedDDLDetail
// extracts. It lives here rather than in a test because the interface
// UnsupportedDDLDetail matches is unexported, so only this package can
// implement it — and a handler test must be able to build one.
type unsupportedDDLError struct {
	object  string
	name    string
	feature string
}

func (e *unsupportedDDLError) Error() string {
	return fmt.Sprintf("database: DDL is unavailable for %s %q: %s", e.object, e.name, e.feature)
}

func (e *unsupportedDDLError) Unwrap() error { return ErrUnsupportedDDL }

func (e *unsupportedDDLError) UnsupportedDDLDetail() (object, name, feature string) {
	return e.object, e.name, e.feature
}

// NewUnsupportedDDLError builds an error that satisfies
// errors.Is(err, ErrUnsupportedDDL) and yields ok == true from
// UnsupportedDDLDetail.
func NewUnsupportedDDLError(object, name, feature string) error {
	return &unsupportedDDLError{object: object, name: name, feature: feature}
}
