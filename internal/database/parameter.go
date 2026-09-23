package database

import (
	"context"
	"database/sql"
)

// ParameterizedRepository is an optional repository capability: a repository
// that can execute or query with positional bound arguments in place of SQL
// text interpolation. Handlers type-assert it rather than checking the driver
// name, so any driver that later implements it gets the behaviour for free.
type ParameterizedRepository interface {
	ExecParams(ctx context.Context, query string, args []any) (sql.Result, error)
	QueryParams(ctx context.Context, query string, args []any) (*sql.Rows, error)
}

// ParameterizedReadOnlyQuerier is the bound-argument counterpart of
// ReadOnlyQuerier: it runs a read statement inside an explicit read-only
// transaction and materialises the whole result, with positional arguments
// forwarded to the driver instead of interpolated into query.
type ParameterizedReadOnlyQuerier interface {
	QueryReadOnlyParams(ctx context.Context, query string, args []any) (*QueryResult, error)
}

// InputDescriptor describes one positional input parameter accepted by a
// prepared statement.
type InputDescriptor struct {
	Kind                      string
	Subtype, Scale, Precision int
	Nullable                  bool
}

// InputDescriber is an optional repository capability for describing the
// positional input parameters accepted by a query.
type InputDescriber interface {
	DescribeInputs(ctx context.Context, query string) ([]InputDescriptor, error)
}
