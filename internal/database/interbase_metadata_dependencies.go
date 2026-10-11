package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sqls-server/sqls/catalog"
	"interbase-go/schema"
)

const (
	interBaseCheckTriggersQuery = `
SELECT %s, %s, %s
FROM RDB$CHECK_CONSTRAINTS cc
JOIN RDB$RELATION_CONSTRAINTS rc ON rc.RDB$CONSTRAINT_NAME = cc.RDB$CONSTRAINT_NAME
WHERE rc.RDB$CONSTRAINT_TYPE = 'CHECK'`
	interBaseComputedFieldsQuery = `
SELECT %s, %s, %s
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE f.RDB$COMPUTED_SOURCE IS NOT NULL`
	interBaseDependenciesQuery = `
SELECT %s, d.RDB$DEPENDENT_TYPE, %s, %s, d.RDB$DEPENDED_ON_TYPE
FROM RDB$DEPENDENCIES d`
)

// readMetadataDependencies reads RDB$DEPENDENCIES in one statement and maps
// raw catalog objects to the ones people recognize where the catalog proves
// it: CHECK_n triggers to their check constraint, RDB$n computed-field
// domains to their column, and relations to tables or views.
func (db *InterBaseDBRepository) readMetadataDependencies(ctx context.Context, q schema.Queryer, width int, cache *DBCache) (MetadataPatch, error) {
	interned := make(map[string]string)
	intern := func(s string) string {
		if v, ok := interned[s]; ok {
			return v
		}
		interned[s] = s
		return s
	}
	id := func(ref string) string { return interBaseMetadataIdentifier(ref, width) }
	names := func(rows *sql.Rows, labels ...string) ([]string, error) {
		raw := make([]sql.NullString, len(labels))
		dest := make([]any, len(labels))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out := make([]string, len(labels))
		for i, label := range labels {
			name, err := interBaseRequiredName(raw[i], label)
			if err != nil {
				return nil, err
			}
			out[i] = intern(name)
		}
		return out, nil
	}

	checks := make(map[string]catalog.DependencyRef)
	checkNames := make(map[string][]string)
	err := interBaseBulkQuery(ctx, q, "check constraint triggers",
		fmt.Sprintf(interBaseCheckTriggersQuery, id("cc.RDB$TRIGGER_NAME"), id("rc.RDB$CONSTRAINT_NAME"), id("rc.RDB$RELATION_NAME")),
		func(rows *sql.Rows) error {
			v, err := names(rows, "check trigger name", "check constraint name", "check relation name")
			if err != nil {
				return err
			}
			checks[v[0]] = catalog.DependencyRef{Kind: catalog.ObjectKindConstraint, Name: v[1], Parent: v[2]}
			checkNames[v[2]] = append(checkNames[v[2]], v[1])
			return nil
		})
	if err != nil {
		return MetadataPatch{}, err
	}

	computed := make(map[string]catalog.DependencyRef)
	err = interBaseBulkQuery(ctx, q, "computed fields",
		fmt.Sprintf(interBaseComputedFieldsQuery, id("rf.RDB$FIELD_SOURCE"), id("rf.RDB$RELATION_NAME"), id("rf.RDB$FIELD_NAME")),
		func(rows *sql.Rows) error {
			v, err := names(rows, "computed field source", "computed field relation", "computed field name")
			if err != nil {
				return err
			}
			computed[v[0]] = catalog.DependencyRef{Kind: catalog.ObjectKindColumn, Name: v[2], Parent: v[1]}
			return nil
		})
	if err != nil {
		return MetadataPatch{}, err
	}

	isView := func(name string) bool { _, ok := cache.ViewByID(name); return ok }
	edges := make([]catalog.DependencyEdge, 0, 1024)
	err = interBaseBulkQuery(ctx, q, "dependencies",
		fmt.Sprintf(interBaseDependenciesQuery, id("d.RDB$DEPENDENT_NAME"), id("d.RDB$FIELD_NAME"), id("d.RDB$DEPENDED_ON_NAME")),
		func(rows *sql.Rows) error {
			var dependent, field, dependedOn sql.NullString
			var dependentType, dependedOnType sql.NullInt64
			if err := rows.Scan(&dependent, &dependentType, &field, &dependedOn, &dependedOnType); err != nil {
				return err
			}
			from, err := interBaseRequiredName(dependent, "dependent name")
			if err != nil {
				return err
			}
			to, err := interBaseRequiredName(dependedOn, "depended-on name")
			if err != nil {
				return err
			}
			edges = append(edges, catalog.DependencyEdge{
				From:  interBaseDependencyRef(dependentType, intern(from), isView, checks, computed),
				To:    interBaseDependencyRef(dependedOnType, intern(to), isView, checks, computed),
				Field: intern(trimInterBaseCatalogName(field).String),
			})
			return nil
		})
	if err != nil {
		return MetadataPatch{}, err
	}
	graph := catalog.NewDependencyGraph(edges, checkNames)
	return MetadataPatch{Cache: &DBCache{Dependencies: graph}, Count: graph.Len()}, nil
}

func interBaseDependencyRef(code sql.NullInt64, name string, isView func(string) bool, checks, computed map[string]catalog.DependencyRef) catalog.DependencyRef {
	if !code.Valid {
		return catalog.DependencyRef{Kind: "type unknown", Name: name}
	}
	kind := schema.DependencyObjectType(code.Int64)
	switch kind {
	case schema.DependencyRelation:
		if isView(name) {
			return catalog.DependencyRef{Kind: catalog.ObjectKindView, Name: name}
		}
		return catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: name}
	case schema.DependencyView:
		return catalog.DependencyRef{Kind: catalog.ObjectKindView, Name: name}
	case schema.DependencyTrigger:
		if ref, ok := checks[name]; ok {
			return ref
		}
		return catalog.DependencyRef{Kind: catalog.ObjectKindTrigger, Name: name}
	case schema.DependencyComputedField:
		if ref, ok := computed[name]; ok {
			return ref
		}
	case schema.DependencyProcedure:
		return catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: name}
	case schema.DependencyException:
		return catalog.DependencyRef{Kind: catalog.ObjectKindException, Name: name}
	case schema.DependencyGenerator:
		return catalog.DependencyRef{Kind: catalog.ObjectKindGenerator, Name: name}
	case schema.DependencyFunction:
		return catalog.DependencyRef{Kind: catalog.ObjectKindFunction, Name: name}
	}
	return catalog.DependencyRef{Kind: catalog.ObjectKind(kind.String()), Name: name}
}
