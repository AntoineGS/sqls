package database

import (
	"context"
	"errors"
	"fmt"

	"interbase-go/schema"
)

var _ DDLRepository = (*InterBaseDBRepository)(nil)

// ObjectDDL reconstructs an object's definition from the catalog using the
// source database dialect reported at connect time (falling back to the
// effective attachment dialect when diagnostics were unavailable). Callers
// must not treat this as cross-dialect conversion: source SQL and defaults
// remain verbatim and are rendered only for the compatible source dialect.
//
// The object is always looked up first, so "no such object" and "the object
// exists but has no renderable DDL" stay distinguishable: the first is
// ErrObjectNotFound, the second ErrUnsupportedDDL. Returning ("", nil) for an
// unknown object would conflate them, and that distinction is the one a caller
// needs in order to choose between showing nothing and showing a reason.
//
// This is an interactive one-shot outside any cache build, so it reads through
// the pooled *sql.DB rather than through a snapshot.
func (db *InterBaseDBRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	if db == nil || db.Conn == nil {
		return "", errors.New("interbase: database connection is nil")
	}
	if name == "" {
		return "", ErrObjectNotFound
	}
	catalog := schema.New(db.Conn)

	var generator schema.DDLer
	switch kind {
	case ObjectKindTable:
		// Table also loads constraints, indexes and triggers, which
		// Relation.GenerateDDL requires.
		object, err := catalog.Table(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindView:
		object, err := catalog.View(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindProcedure:
		object, err := catalog.Procedure(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindTrigger:
		object, err := catalog.Trigger(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindDomain:
		object, err := catalog.Domain(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindIndex:
		object, err := catalog.Index(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindGenerator:
		// InterBase's own DDL for this catalog object is CREATE GENERATOR.
		object, err := catalog.Generator(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindFunction:
		// An external function's creation semantics include calling-convention
		// policy the catalog does not record, so GenerateDDL always refuses.
		// The lookup still runs, so an unknown name is ErrObjectNotFound.
		object, err := catalog.Function(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	default:
		return "", fmt.Errorf("interbase: unsupported object kind %q", kind)
	}

	options, ok := generator.(interface {
		GenerateDDLWithOptions(schema.DDLOptions) (string, error)
	})
	if !ok {
		// External functions are intentionally unsupported and expose no
		// options-aware DDL method. Keep their structured refusal intact.
		ddl, err := generator.GenerateDDL()
		if err != nil {
			return "", interBaseWrapDDLError(err)
		}
		return ddl, nil
	}
	ddl, err := options.GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseSourceDialect(db)})
	if err != nil {
		return "", interBaseWrapDDLError(err)
	}
	return ddl, nil
}

func interBaseSourceDialect(db *InterBaseDBRepository) schema.DDLDialect {
	dialect := db.SourceSQLDialect
	if dialect == 0 {
		dialect = db.SQLDialect
	}
	if dialect != 1 && dialect != 3 {
		dialect = 3
	}
	return schema.DDLDialect(dialect)
}

// interBaseUnsupportedDDL adapts the driver's structured refusal to the
// driver-neutral contract: errors.Is reaches the shared sentinel through
// Unwrap, and UnsupportedDDLDetail reaches the reason through the method,
// so capability.go never imports schema.
type interBaseUnsupportedDDL struct{ detail *schema.UnsupportedDDLError }

func (e *interBaseUnsupportedDDL) Error() string { return "interbase: " + e.detail.Error() }
func (e *interBaseUnsupportedDDL) Unwrap() error { return ErrUnsupportedDDL }
func (e *interBaseUnsupportedDDL) UnsupportedDDLDetail() (string, string, string) {
	return e.detail.Object, e.detail.Name, e.detail.Feature
}

// interBaseWrapDDLError maps a driver refusal onto ErrUnsupportedDDL, keeping
// the structured reason when the driver supplied one. Any other error is a
// real catalog fault and passes through unchanged.
func interBaseWrapDDLError(err error) error {
	if err == nil || !errors.Is(err, schema.ErrUnsupportedDDL) {
		return err
	}
	var detail *schema.UnsupportedDDLError
	if errors.As(err, &detail) && detail != nil {
		return &interBaseUnsupportedDDL{detail: detail}
	}
	return fmt.Errorf("interbase: %w", ErrUnsupportedDDL)
}
