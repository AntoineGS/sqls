package database

import (
	"context"
	"sync"

	"github.com/sqls-server/sqls/dialect"
)

// ObjectDDLCall records one ObjectDDL request made against the mock.
type ObjectDDLCall struct {
	Kind ObjectKind
	Name string
}

// MockCapabilityRepository is a DBRepository that also implements the optional
// InterBase capability interfaces.
//
// It is deliberately a separate type from MockDBRepository rather than extra
// fields on it. Every handler test builds a MockDBRepository; if that type
// satisfied DDLRepository and ExplainRepository, the type assertions in the
// hover and explain paths would succeed everywhere and then call a nil func
// field.
type MockCapabilityRepository struct {
	*MockDBRepository

	MockObjectDDL   func(context.Context, ObjectKind, string) (string, error)
	MockExplainPlan func(context.Context, string) (string, error)

	mu               sync.Mutex
	objectDDLCalls   []ObjectDDLCall
	explainPlanCalls []string
}

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

func (m *MockCapabilityRepository) Driver() dialect.DatabaseDriver {
	return dialect.DatabaseDriverInterBase
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
