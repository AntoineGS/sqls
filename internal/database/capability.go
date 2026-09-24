package database

import (
	"context"
	"errors"
)

// CatalogRepository is implemented by repositories that can enumerate catalog
// objects beyond tables and columns. All methods return objects for the whole
// attachment; sqls drivers without a schema namespace use an empty Schema.
type CatalogRepository interface {
	DescribeViews(ctx context.Context) ([]*ViewDesc, error)
	DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)
	DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error)
	DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error)
	DescribeDomains(ctx context.Context) ([]*DomainDesc, error)
	DescribeIndexes(ctx context.Context) ([]*IndexDesc, error)
	DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error)
}

// DDLRepository is implemented by repositories that can reproduce an object's
// definition. Callers must handle ErrObjectNotFound and ErrUnsupportedDDL.
type DDLRepository interface {
	ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)
}

// TableDescriptionRepository is an optional capability for repositories that
// can return a non-executable, catalog-derived description of an exact table.
type TableDescriptionRepository interface {
	TableDescription(ctx context.Context, name string) (TableDescription, error)
}

// ExplainRepository is implemented by repositories that can return a server
// query plan without executing the statement's result set.
type ExplainRepository interface {
	ExplainPlan(ctx context.Context, query string) (string, error)
}

// CatalogSnapshotRepository is implemented by repositories that can serve a
// whole cache build from one consistent catalog read. The returned repository
// is read-only and valid until close is called; the source repository is
// unaffected and remains usable concurrently.
type CatalogSnapshotRepository interface {
	CatalogSnapshot(ctx context.Context) (repo DBRepository, close func() error, err error)
}

var (
	// ErrObjectNotFound reports that the named catalog object does not exist.
	ErrObjectNotFound = errors.New("database: catalog object not found")
	// ErrUnsupportedDDL reports that the catalog cannot reproduce the object's
	// definition faithfully. Use UnsupportedDDLDetail for the structured reason.
	ErrUnsupportedDDL = errors.New("database: DDL is unavailable for this object")
)

// unsupportedDDLDetailer is implemented by driver-specific errors that carry a
// structured reason. It keeps this file free of any driver import.
type unsupportedDDLDetailer interface {
	UnsupportedDDLDetail() (object, name, feature string)
}

// UnsupportedDDLDetail reports the structured reason behind an
// ErrUnsupportedDDL error: the object kind, the object name, and the metadata
// facet that could not be rendered. ok is false when err carries no detail.
func UnsupportedDDLDetail(err error) (object, name, feature string, ok bool) {
	var detailer unsupportedDDLDetailer
	if !errors.As(err, &detailer) {
		return "", "", "", false
	}
	object, name, feature = detailer.UnsupportedDDLDetail()
	return object, name, feature, true
}
