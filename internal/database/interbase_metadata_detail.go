package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

// MetadataDetailPlan adapts detail requests onto InterBase's retained
// bulk-category facts where possible. Most facets only resolve the requested
// object against an already-ready category and perform pure per-request
// rendering. Selected system columns, generator values and table DDL use
// bounded provider-owned reads in Load; all other renderers use the checked
// category snapshot.
//
// Table DDL is the narrow provider-owned exception: schema.Table performs
// bounded, context-aware reads for that exact relation's constraints,
// indexes and triggers, then renders using the repository's source dialect.
// Its rendered text is cached with dialect-specific coverage. Selected
// generator values are volatile detail reads coalesced only while in flight.
func (db *InterBaseDBRepository) MetadataDetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if req.Consistency == metadata.TransactionLocal || req.Consistency == metadata.ConsistentSnapshot {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	switch req.Facet {
	case "columns":
		return db.detailPlanColumns(req)
	case "description":
		return db.detailPlanDescription(req)
	case "source":
		return db.detailPlanSource(req)
	case "ddl":
		return db.detailPlanDDL(req)
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

func detailCoverage(object catalog.ObjectID, facet string) string {
	return object.Key() + "\x00" + facet
}

// interBaseByIDKey is the *ByID map key an InterBase bulk category job
// inserts one exact-identity object's descriptor at. It is the single
// source both insertion (in the *_jobs.go/relations.go/procedures.go/
// indexes.go/functions.go bulk readers) and lookup (interBaseResolveObjectName)
// derive their key from, so they can never drift apart.
func interBaseByIDKey(kind ObjectKind, name string) string {
	return catalog.ObjectID{Kind: kind, Name: name}.Key()
}

// interBaseResolveObjectName resolves want against names (each object's
// exact catalog casing) for kind, building a fresh catalog.ObjectIndex so
// this exact-identity-then-unquoted-lookup path (Ruling R6) is exercised for
// real InterBase data. An exact case match is returned unconditionally, even
// if it also happens to fold-collide with another name: exact identity is
// never weaker than the unquoted fallback. Otherwise, a unique folded match
// resolves; zero or more than one is reported explicitly rather than picking
// one arbitrarily.
func interBaseResolveObjectName(kind ObjectKind, names []string, want string) (string, error) {
	idx := catalog.NewObjectIndex(strings.ToUpper)
	for _, name := range names {
		idx.Insert(catalog.ObjectID{Kind: kind, Name: name})
	}
	if _, ok := idx.Lookup(catalog.ObjectID{Kind: kind, Name: want}); ok {
		return want, nil
	}
	matches := idx.LookupUnquoted(kind, want)
	switch len(matches) {
	case 0:
		return "", ErrObjectNotFound
	case 1:
		return matches[0].Name, nil
	default:
		return "", fmt.Errorf("interbase: object name %q is ambiguous among %d matches: %w", want, len(matches), catalog.ErrAmbiguous)
	}
}

func interBaseResolveRelationName(cache *DBCache, object catalog.ObjectID) (string, error) {
	if cache == nil {
		return "", ErrObjectNotFound
	}
	return interBaseResolveObjectName(object.Kind, cache.SchemaTables[""], object.Name)
}

// interBaseResolveCatalogName resolves want against cache.Catalog.Objects for
// kind (Ruling R6's exact-identity-then-unquoted-lookup path), never through
// a legacy upper-cased CatalogCache map: two exact-identity objects that
// fold to the same case (for example a quoted-mixed-case name and its
// all-upper-case counterpart) can otherwise silently collapse to one entry
// before a name resolution even runs. An ambiguous unquoted match is
// reported explicitly rather than picking one of the matches arbitrarily.
func interBaseResolveCatalogName(cache *DBCache, kind ObjectKind, want string) (string, error) {
	if cache == nil {
		return "", ErrObjectNotFound
	}
	name, found, ambiguous := cache.ResolveObjectName(kind, want)
	if len(ambiguous) > 0 {
		return "", fmt.Errorf("interbase: object name %q is ambiguous among %d matches: %w", want, len(ambiguous), catalog.ErrAmbiguous)
	}
	if !found {
		return "", ErrObjectNotFound
	}
	return name, nil
}

func interBaseResolveView(cache *DBCache, object catalog.ObjectID) (string, *ViewDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	view, ok := cache.ViewByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, view, nil
}

func interBaseResolveProcedure(cache *DBCache, object catalog.ObjectID) (string, *ProcedureDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	procedure, ok := cache.ProcedureByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, procedure, nil
}

func interBaseResolveDomain(cache *DBCache, object catalog.ObjectID) (string, *DomainDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	domain, ok := cache.DomainByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, domain, nil
}

func interBaseResolveTrigger(cache *DBCache, object catalog.ObjectID) (string, *TriggerDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	trigger, ok := cache.TriggerByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, trigger, nil
}

func interBaseResolveIndex(cache *DBCache, object catalog.ObjectID) (string, *IndexDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	index, ok := cache.IndexByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, index, nil
}

func interBaseResolveGenerator(cache *DBCache, object catalog.ObjectID) (string, *GeneratorDesc, error) {
	name, err := interBaseResolveCatalogName(cache, object.Kind, object.Name)
	if err != nil {
		return "", nil, err
	}
	generator, ok := cache.GeneratorByID(name)
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	return name, generator, nil
}

func interBaseResolveSystemRelation(cache *DBCache, object catalog.ObjectID) (string, *SystemRelationDesc, error) {
	if cache == nil || cache.Catalog == nil {
		return "", nil, ErrObjectNotFound
	}
	names := make([]string, 0, len(cache.Catalog.SystemRelationsByID))
	for _, relation := range cache.Catalog.SystemRelationsByID {
		if relation != nil {
			names = append(names, relation.Name)
		}
	}
	name, err := interBaseResolveObjectName(ObjectKindTable, names, object.Name)
	if err != nil {
		return "", nil, err
	}
	relation, ok := cache.SystemRelationByID(name)
	if !ok || relation == nil || !relation.IsSystem {
		return "", nil, ErrObjectNotFound
	}
	return name, relation, nil
}

func interBaseResolveException(cache *DBCache, object catalog.ObjectID) (string, *ExceptionDesc, error) {
	name, err := interBaseResolveCatalogName(cache, ObjectKindException, object.Name)
	if err != nil {
		return "", nil, err
	}
	exception, ok := cache.ExceptionByID(name)
	if !ok || exception == nil {
		return "", nil, ErrObjectNotFound
	}
	return name, exception, nil
}

func interBaseResolveFunction(cache *DBCache, object catalog.ObjectID) (string, *FunctionDesc, error) {
	name, err := interBaseResolveCatalogName(cache, ObjectKindFunction, object.Name)
	if err != nil {
		return "", nil, err
	}
	function, ok := cache.FunctionByID(name)
	if !ok || function == nil {
		return "", nil, ErrObjectNotFound
	}
	return name, function, nil
}

// --- facet: columns -------------------------------------------------------

func (db *InterBaseDBRepository) detailPlanColumns(req metadata.Request) (metadata.DetailPlan, error) {
	if req.Object.Kind == ObjectKindTable && req.IsSystem {
		return db.detailPlanSystemColumns(req)
	}
	return metadata.DetailPlan{
		Requires: []MetadataKind{MetadataRelations, MetadataColumnsCurrent},
		Coverage: detailCoverage(req.Object, "columns"),
		Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
			name, err := interBaseResolveRelationName(cache, req.Object)
			if err != nil {
				return metadata.Detail{}, err
			}
			// ColumnDescsByID, not ColumnDescs: the latter is keyed by
			// ColumnDatabaseKey's shared upper-cased name, so two
			// exact-identity relations that fold to the same case (a
			// quoted "Foo" alongside FOO) would otherwise silently merge
			// their columns here, even though name was already resolved
			// to one exact identity above.
			columns, _ := cache.ColumnDescsByID(name)
			return metadata.Detail{Object: req.Object, Columns: columns}, nil
		},
	}, nil
}

func (db *InterBaseDBRepository) detailPlanSystemColumns(req metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{
		Requires: []MetadataKind{MetadataSystemRelations},
		Coverage: detailCoverage(req.Object, "columns") + "\x00system",
		CoverageForCache: func(cache *catalog.Cache) (string, error) {
			name, _, err := interBaseResolveSystemRelation(cache, req.Object)
			if err != nil {
				return "", err
			}
			object := catalog.ObjectID{Kind: ObjectKindTable, Name: name}
			return detailCoverage(object, "columns") + "\x00system", nil
		},
		Load: func(ctx context.Context, cache *DBCache) (metadata.Detail, error) {
			name, _, err := interBaseResolveSystemRelation(cache, req.Object)
			if err != nil {
				return metadata.Detail{}, err
			}
			var relation *schema.Relation
			if err := db.withReadOnlyDetailTransaction(ctx, func(tx *sql.Tx) error {
				var err error
				relation, err = schema.New(tx).SystemRelation(ctx, name)
				return err
			}); err != nil {
				return metadata.Detail{}, err
			}
			if relation == nil {
				return metadata.Detail{}, ErrObjectNotFound
			}
			if !relation.SystemFlag.Valid || relation.SystemFlag.Int64 == 0 {
				return metadata.Detail{}, ErrObjectNotFound
			}
			columns := make([]*ColumnDesc, 0, len(relation.Columns))
			for _, column := range relation.Columns {
				columns = append(columns, db.columnDescription(relation.Name, column, nil))
			}
			return metadata.Detail{Object: catalog.ObjectID{Kind: ObjectKindTable, Name: name}, Columns: columns}, nil
		},
	}, nil
}

func (db *InterBaseDBRepository) withReadOnlyDetailTransaction(ctx context.Context, read func(*sql.Tx) error) error {
	if db == nil || db.Conn == nil {
		return fmt.Errorf("interbase: database connection is nil")
	}
	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// --- facet: description ----------------------------------------------------

func (db *InterBaseDBRepository) detailPlanDescription(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case ObjectKindTable:
		if req.IsSystem {
			return metadata.DetailPlan{}, metadata.ErrUnsupported
		}
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataRelations, MetadataColumnsCurrent},
			Coverage: detailCoverage(req.Object, "description"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, err := interBaseResolveRelationName(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, err := interBaseResolveRelationName(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				// See detailPlanColumns: exact-identity lookup, not the
				// shared upper-cased ColumnDescs.
				columns, _ := cache.ColumnDescsByID(name)
				relation := interBaseSchemaRelationTable(name, columns)
				described, err := relation.DescribeCatalogWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, err
				}
				acquired.Columns = columns
				acquired.Description = copyCatalogDescription(described)
				return acquired, nil
			},
		}, nil
	case ObjectKindView:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataViews},
			Coverage: detailCoverage(req.Object, "description"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveView(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, view, err := interBaseResolveView(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				relation := interBaseSchemaRelationView(name, view)
				described, err := relation.DescribeCatalogWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, err
				}
				acquired.Columns = view.Columns
				acquired.Description = copyCatalogDescription(described)
				return acquired, nil
			},
		}, nil
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

// --- facet: source ----------------------------------------------------------

func (db *InterBaseDBRepository) detailPlanSource(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case ObjectKindProcedure:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataProcedures},
			Coverage: detailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				_, procedure, err := interBaseResolveProcedure(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object, Text: procedure.Source.String, Provenance: "interbase:procedure-source"}, nil
			},
		}, nil
	case ObjectKindView:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataViews},
			Coverage: detailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				_, view, err := interBaseResolveView(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object, Text: view.ViewSource.String, Provenance: "interbase:view-source"}, nil
			},
		}, nil
	case ObjectKindTrigger:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataTriggers},
			Coverage: detailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				_, trigger, err := interBaseResolveTrigger(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object, Text: trigger.Source.String, Provenance: "interbase:trigger-source"}, nil
			},
		}, nil
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

// --- facet: ddl ---------------------------------------------------------

func (db *InterBaseDBRepository) detailPlanDDL(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case ObjectKindProcedure:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataProcedures},
			Coverage: detailCoverage(req.Object, "ddl"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveProcedure(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				_, procedure, err := interBaseResolveProcedure(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaProcedure(procedure).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = db.formatDDLHeader(ddl, procedure.Source.String, req.Dialect)
				acquired.Provenance = "interbase:procedure-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindDomain:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataDomains},
			Coverage: detailCoverage(req.Object, "ddl"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveDomain(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				_, domain, err := interBaseResolveDomain(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaDomain(domain).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
				acquired.Provenance = "interbase:domain-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindTrigger:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataTriggers},
			Coverage: detailCoverage(req.Object, "ddl"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveTrigger(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				_, trigger, err := interBaseResolveTrigger(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaTrigger(trigger).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
				acquired.Provenance = "interbase:trigger-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindIndex:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataIndexes},
			Coverage: detailCoverage(req.Object, "ddl"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveIndex(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				_, index, err := interBaseResolveIndex(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaIndex(index).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
				acquired.Provenance = "interbase:index-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindGenerator:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataGenerators},
			Coverage: detailCoverage(req.Object, "ddl"),
			Volatile: true,
			CoverageForCache: func(cache *catalog.Cache) (string, error) {
				name, _, err := interBaseResolveGenerator(cache, req.Object)
				if err != nil {
					return "", err
				}
				object := catalog.ObjectID{Kind: ObjectKindGenerator, Name: name}
				return detailCoverage(object, "ddl"), nil
			},
			Load: func(ctx context.Context, cache *DBCache) (metadata.Detail, error) {
				name, _, err := interBaseResolveGenerator(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				value, err := db.generatorValueWithDetailTransaction(ctx, name)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{
					Object: catalog.ObjectID{Kind: ObjectKindGenerator, Name: name},
					Text:   strconv.FormatInt(value, 10),
				}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, generator, err := interBaseResolveGenerator(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaSequence(generator).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				identifier, err := interBaseGeneratedIdentifier(ddl, "CREATE GENERATOR ")
				if err != nil {
					return metadata.Detail{}, err
				}
				if acquired.Text == "" {
					return metadata.Detail{}, metadata.ErrUnsupported
				}
				acquired.Object = catalog.ObjectID{Kind: ObjectKindGenerator, Name: name}
				acquired.Text = ddl + "\nSET GENERATOR " + identifier + " TO " + acquired.Text + ";"
				acquired.Provenance = "interbase:generator-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindException:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataExceptions},
			Coverage: detailCoverage(req.Object, "ddl"),
			CoverageForCache: func(cache *catalog.Cache) (string, error) {
				name, _, err := interBaseResolveException(cache, req.Object)
				if err != nil {
					return "", err
				}
				object := catalog.ObjectID{Kind: ObjectKindException, Name: name}
				return detailCoverage(object, "ddl"), nil
			},
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				name, _, err := interBaseResolveException(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: catalog.ObjectID{Kind: ObjectKindException, Name: name}}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, exception, err := interBaseResolveException(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				if !exception.Message.Valid {
					return metadata.Detail{}, metadata.ErrUnsupported
				}
				sequenceDDL, err := interBaseSchemaSequence(&GeneratorDesc{Name: name}).GenerateDDLWithOptions(
					db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, metadata.ErrUnsupported
				}
				identifier, err := interBaseGeneratedIdentifier(sequenceDDL, "CREATE GENERATOR ")
				if err != nil {
					return metadata.Detail{}, err
				}
				acquired.Object = catalog.ObjectID{Kind: ObjectKindException, Name: name}
				acquired.Text = "CREATE EXCEPTION " + identifier + " '" + strings.ReplaceAll(exception.Message.String, "'", "''") + "';"
				acquired.Provenance = "interbase:exception-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindFunction:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataFunctions},
			Coverage: detailCoverage(req.Object, "ddl"),
			CoverageForCache: func(cache *catalog.Cache) (string, error) {
				name, _, err := interBaseResolveFunction(cache, req.Object)
				if err != nil {
					return "", err
				}
				object := catalog.ObjectID{Kind: ObjectKindFunction, Name: name}
				return detailCoverage(object, "ddl"), nil
			},
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				name, _, err := interBaseResolveFunction(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: catalog.ObjectID{Kind: ObjectKindFunction, Name: name}}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, function, err := interBaseResolveFunction(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaFunction(function).GenerateDDL()
				if err != nil {
					if errors.Is(err, schema.ErrUnsupportedDDL) {
						return metadata.Detail{}, metadata.ErrUnsupported
					}
					return metadata.Detail{}, err
				}
				acquired.Object = catalog.ObjectID{Kind: ObjectKindFunction, Name: name}
				acquired.Text = ddl
				acquired.Provenance = "interbase:function-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindView:
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataViews},
			Coverage: detailCoverage(req.Object, "ddl"),
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveView(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				name, view, err := interBaseResolveView(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaRelationView(name, view).GenerateDDLWithOptions(db.ddlOptions(interBaseDDLDialectFor(req.Dialect)))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = db.formatDDLHeader(ddl, view.ViewSource.String, req.Dialect)
				acquired.Provenance = "interbase:view-ddl"
				return acquired, nil
			},
		}, nil
	case ObjectKindTable:
		if req.IsSystem {
			return metadata.DetailPlan{}, metadata.ErrUnsupported
		}
		// schema.Table needs to fetch constraints/indexes/triggers and its DDL
		// is dialect-specific. Canonicalize the request using the ready cache
		// in CoverageForCache, then include the effective source dialect in
		// the retained key so equivalent spellings coalesce without sharing
		// text across dialect variants.
		dialect := interBaseSourceDialect(db)
		return metadata.DetailPlan{
			Requires: []MetadataKind{MetadataRelations},
			Coverage: detailCoverage(req.Object, "ddl"),
			CoverageForCache: func(cache *catalog.Cache) (string, error) {
				name, err := interBaseResolveRelationName(cache, req.Object)
				if err != nil {
					return "", err
				}
				canonicalObject := catalog.ObjectID{Kind: req.Object.Kind, Name: name}
				return detailCoverage(canonicalObject, "ddl") + "\x00source-dialect:" + fmt.Sprint(dialect), nil
			},
			Load: func(ctx context.Context, cache *DBCache) (metadata.Detail, error) {
				name, err := interBaseResolveRelationName(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				canonicalObject := catalog.ObjectID{Kind: req.Object.Kind, Name: name}
				if db == nil || db.Conn == nil {
					return metadata.Detail{}, fmt.Errorf("interbase: database connection is nil")
				}
				var queryer schema.Queryer = db.Conn
				var tx *sql.Tx
				if db.Firebird {
					tx, err = db.Conn.BeginTx(ctx, &sql.TxOptions{
						ReadOnly:  true,
						Isolation: sql.LevelReadCommitted,
					})
					if err != nil {
						return metadata.Detail{}, err
					}
					// Firebird metadata attachments require explicit transactions.
					// Roll back immediately after materializing the catalog result;
					// keep the historical InterBase implicit-read path unchanged.
					defer func() { _ = tx.Rollback() }()
					queryer = tx
				}
				table, err := schema.New(queryer).Table(ctx, canonicalObject.Name)
				if tx != nil {
					if rollbackErr := tx.Rollback(); err == nil {
						err = rollbackErr
					}
				}
				if err != nil {
					return metadata.Detail{}, err
				}
				if table == nil {
					return metadata.Detail{}, ErrObjectNotFound
				}
				ddl, err := table.GenerateDDLWithOptions(db.ddlOptions(dialect))
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				return metadata.Detail{Object: canonicalObject, Text: ddl, Provenance: "interbase:table-ddl"}, nil
			},
		}, nil
	default:
		// Unsupported object kinds must not trigger a full-catalog refetch.
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

// --- retained-fact -> driver value reconstruction --------------------------

// formatDDLHeader lays out the header of ddl, which ends with the stored
// body, through FormatDDLHeader and appends the body verbatim.
func (db *InterBaseDBRepository) formatDDLHeader(ddl, body string, dialect int) string {
	if db.FormatDDLHeader == nil || !strings.HasSuffix(ddl, body) {
		return ddl
	}
	if dialect != 1 {
		dialect = 3
	}
	header, err := db.FormatDDLHeader(strings.TrimRightFunc(strings.TrimSuffix(ddl, body), unicode.IsSpace), dialect)
	if err != nil {
		return ddl
	}
	if strings.TrimLeftFunc(body, unicode.IsSpace) == body {
		header += " "
	}
	return header + body
}

func interBaseDDLDialectFor(dialect int) schema.DDLDialect {
	if dialect == 1 {
		return schema.Dialect1
	}
	return schema.Dialect3
}

func interBaseGeneratedIdentifier(statement, prefix string) (string, error) {
	if !strings.HasPrefix(statement, prefix) {
		return "", fmt.Errorf("interbase: generated statement %q has no %q identifier prefix", statement, prefix)
	}
	identifier := strings.TrimPrefix(statement, prefix)
	if identifier == "" {
		return "", metadata.ErrUnsupported
	}
	return identifier, nil
}

func (db *InterBaseDBRepository) generatorValueWithDetailTransaction(ctx context.Context, name string) (int64, error) {
	var value int64
	err := db.withReadOnlyDetailTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		value, err = schema.New(tx).GeneratorValueWithOptions(ctx, name, db.ddlOptions(interBaseDDLDialectFor(db.SQLDialect)))
		return err
	})
	if err != nil {
		return 0, err
	}
	return value, nil
}

func interBaseSchemaDomainFromFacts(facts *ColumnTypeFacts) *schema.Domain {
	if facts == nil {
		return nil
	}
	return &schema.Domain{
		Name:             facts.DomainName.String,
		ValidationSource: facts.ValidationSource,
		ComputedSource:   facts.ComputedSource,
		DefaultSource:    facts.DomainDefault,
		FieldLength:      facts.FieldLength,
		FieldScale:       facts.FieldScale,
		FieldType:        facts.FieldType,
		FieldSubType:     facts.FieldSubType,
		SystemFlag:       interBaseBoolFlag(facts.SystemDomain),
		CharacterLength:  facts.CharacterLength,
		CollationID:      facts.CollationID,
		CharacterSetID:   facts.CharacterSetID,
		FieldPrecision:   facts.FieldPrecision,
		CharacterSetName: facts.CharacterSetName,
		CollationName:    facts.CollationName,
		Dimensions:       facts.Dimensions,
	}
}

func interBaseBoolFlag(set bool) sql.NullInt64 {
	if set {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

func interBaseSchemaColumn(desc *ColumnDesc) schema.Column {
	column := schema.Column{
		Name:          desc.Name,
		RelationName:  desc.Table,
		Nullable:      interBaseNullableFromString(desc.Null),
		DefaultSource: desc.Default,
	}
	if desc.Facts != nil {
		column.Domain = interBaseSchemaDomainFromFacts(desc.Facts)
		column.FieldSource = desc.Facts.DomainName
		column.ComputedSource = desc.Facts.ComputedSource
		column.CollationID = desc.Facts.ColumnCollationID
		column.CollationName = desc.Facts.ColumnCollationName
	}
	return column
}

func interBaseNullableFromString(null string) sql.NullBool {
	switch null {
	case "YES":
		return sql.NullBool{Bool: true, Valid: true}
	case "NO":
		return sql.NullBool{Bool: false, Valid: true}
	default:
		return sql.NullBool{}
	}
}

func interBaseSchemaRelationTable(name string, columns []*ColumnDesc) schema.Relation {
	cols := make([]schema.Column, 0, len(columns))
	for _, column := range columns {
		cols = append(cols, interBaseSchemaColumn(column))
	}
	return schema.Relation{Name: name, Kind: schema.RelationTable, Columns: cols, ConstraintsLoaded: false}
}

func interBaseSchemaRelationView(name string, view *ViewDesc) schema.Relation {
	cols := make([]schema.Column, 0, len(view.Columns))
	for _, column := range view.Columns {
		cols = append(cols, interBaseSchemaColumn(column))
	}
	return schema.Relation{Name: name, Kind: schema.RelationView, ViewSource: view.ViewSource, Columns: cols}
}

func interBaseSchemaParameter(desc *ProcedureParameterDesc, procedureName string, direction schema.ParameterDirection) schema.ProcedureParameter {
	parameterType := sql.NullInt64{Int64: 0, Valid: true}
	if direction == schema.ParameterOutput {
		parameterType = sql.NullInt64{Int64: 1, Valid: true}
	}
	parameter := schema.ProcedureParameter{
		Name:                       desc.Name,
		ProcedureName:              procedureName,
		Number:                     sql.NullInt64{Int64: int64(desc.Position), Valid: true},
		Direction:                  direction,
		ParameterType:              parameterType,
		Nullable:                   desc.Nullable,
		FirebirdParameterMechanism: desc.FirebirdParameterMechanism,
		FirebirdTypeOfFieldName:    desc.FirebirdTypeOfFieldName,
		FirebirdTypeOfRelationName: desc.FirebirdTypeOfRelationName,
	}
	if desc.Facts != nil {
		parameter.Domain = interBaseSchemaDomainFromFacts(desc.Facts)
		parameter.FieldSource = desc.Facts.DomainName
		// InterBase's RDB$PROCEDURE_PARAMETERS has no null flag: a resolved
		// domain that is not NOT NULL makes the parameter nullable.
		if !parameter.Nullable.Valid {
			parameter.Nullable = sql.NullBool{Bool: true, Valid: true}
		}
	}
	return parameter
}

func interBaseSchemaProcedure(desc *ProcedureDesc) schema.Procedure {
	procedure := schema.Procedure{
		Name:        desc.Name,
		Source:      desc.Source,
		InputCount:  sql.NullInt64{Int64: int64(len(desc.InputParameters)), Valid: true},
		OutputCount: sql.NullInt64{Int64: int64(len(desc.OutputParameters)), Valid: true},
	}
	for _, parameter := range desc.InputParameters {
		procedure.InputParameters = append(procedure.InputParameters, interBaseSchemaParameter(parameter, desc.Name, schema.ParameterInput))
	}
	for _, parameter := range desc.OutputParameters {
		procedure.OutputParameters = append(procedure.OutputParameters, interBaseSchemaParameter(parameter, desc.Name, schema.ParameterOutput))
	}
	return procedure
}

func interBaseSchemaFunction(desc *FunctionDesc) schema.Function {
	function := schema.Function{
		Name:           desc.Name,
		FunctionType:   desc.FunctionType,
		Description:    desc.Description,
		ModuleName:     desc.ModuleName,
		EntryPoint:     desc.EntryPoint,
		ReturnArgument: desc.ReturnPosition,
		SystemFlag:     desc.SystemFlag,
		Arguments:      make([]schema.FunctionArgument, 0, len(desc.Arguments)),
	}
	for _, argument := range desc.Arguments {
		if argument == nil {
			function.Arguments = append(function.Arguments, schema.FunctionArgument{})
			continue
		}
		function.Arguments = append(function.Arguments, schema.FunctionArgument{
			Name:            argument.Name,
			FunctionName:    argument.FunctionName,
			Position:        argument.Position,
			Mechanism:       argument.Mechanism,
			FieldLength:     argument.FieldLength,
			FieldScale:      argument.FieldScale,
			FieldType:       argument.FieldType,
			FieldSubType:    argument.FieldSubType,
			CharacterSetID:  argument.CharacterSetID,
			FieldPrecision:  argument.FieldPrecision,
			CharacterLength: argument.CharacterLength,
		})
	}
	return function
}

func interBaseSchemaDomain(desc *DomainDesc) schema.Domain {
	domain := interBaseSchemaDomainFromFacts(desc.Facts)
	if domain == nil {
		domain = &schema.Domain{Name: desc.Name}
	}
	// DomainDesc.Nullable is the driver's already-decoded "is nullable"
	// fact (see schema.nullable); the standalone CREATE DOMAIN renderer
	// needs it back in its raw NullFlag form (0 nullable, nonzero NOT
	// NULL), which ColumnTypeFacts does not separately carry.
	if desc.Nullable.Valid {
		domain.NullFlag = interBaseActiveToInactiveFlag(desc.Nullable)
	}
	return *domain
}

func interBaseActiveToInactiveFlag(active sql.NullBool) sql.NullInt64 {
	if !active.Valid {
		return sql.NullInt64{}
	}
	if active.Bool {
		return sql.NullInt64{Int64: 0, Valid: true}
	}
	return sql.NullInt64{Int64: 1, Valid: true}
}

func interBaseSchemaTrigger(desc *TriggerDesc) schema.Trigger {
	var relationName sql.NullString
	if desc.RelationName.Valid {
		relationName = desc.RelationName
	}
	return schema.Trigger{
		Name:         desc.Name,
		RelationName: relationName,
		Sequence:     desc.Sequence,
		TriggerType:  desc.TypeCode,
		Source:       desc.Source,
		Inactive:     interBaseActiveToInactiveFlag(desc.Active),
	}
}

func interBaseUniqueFlag(unique sql.NullBool) sql.NullInt64 {
	if !unique.Valid {
		return sql.NullInt64{}
	}
	if unique.Bool {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

func interBaseSchemaIndex(desc *IndexDesc) schema.Index {
	segments := make([]schema.IndexSegment, 0, len(desc.Columns))
	for _, column := range desc.Columns {
		segments = append(segments, schema.IndexSegment{IndexName: desc.Name, FieldName: column})
	}
	return schema.Index{
		Name:         desc.Name,
		RelationName: desc.RelationName,
		UniqueFlag:   interBaseUniqueFlag(desc.Unique),
		Inactive:     interBaseActiveToInactiveFlag(desc.Active),
		IndexType:    desc.Direction,
		Expression:   desc.Expression,
		Segments:     segments,
	}
}

func interBaseSchemaSequence(desc *GeneratorDesc) schema.Sequence {
	return schema.Sequence{Name: desc.Name, ID: desc.ID}
}
