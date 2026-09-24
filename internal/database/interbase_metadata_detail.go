package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

// MetadataDetailPlan adapts one detail request onto InterBase's retained
// bulk-category facts. Every facet implemented here is satisfiable entirely
// from data the existing MetadataPlan bulk jobs already loaded: Load only
// resolves the requested object's exact name against the already-ready
// category (never issuing SQL), and any dialect-sensitive rendering runs in
// Render, which is pure and receives the current cache plus the per-request
// Dialect. This is what lets a hover-then-explorer (or explorer-then-hover)
// sequence, or two DDL requests at different dialects, share one Load.
//
// Table and external-function DDL are not implemented in this increment:
// full CREATE TABLE reconstruction needs constraint/index/trigger facts no
// bulk category retains, and the driver's own Function.GenerateDDL always
// refuses. Both return metadata.ErrUnsupported rather than silently
// re-fetching the whole catalog or baking one dialect into a shared Load.
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

// --- facet: columns -------------------------------------------------------

func (db *InterBaseDBRepository) detailPlanColumns(req metadata.Request) (metadata.DetailPlan, error) {
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

// --- facet: description ----------------------------------------------------

func (db *InterBaseDBRepository) detailPlanDescription(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case ObjectKindTable:
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
				described, err := relation.DescribeCatalogWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
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
				described, err := relation.DescribeCatalogWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
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
				ddl, err := interBaseSchemaProcedure(procedure).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
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
				ddl, err := interBaseSchemaDomain(domain).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
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
				ddl, err := interBaseSchemaTrigger(trigger).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
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
				ddl, err := interBaseSchemaIndex(index).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
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
			Load: func(_ context.Context, cache *DBCache) (metadata.Detail, error) {
				if _, _, err := interBaseResolveGenerator(cache, req.Object); err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: req.Object}, nil
			},
			Render: func(cache *DBCache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
				_, generator, err := interBaseResolveGenerator(cache, req.Object)
				if err != nil {
					return metadata.Detail{}, err
				}
				ddl, err := interBaseSchemaSequence(generator).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
				acquired.Provenance = "interbase:generator-ddl"
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
				ddl, err := interBaseSchemaRelationView(name, view).GenerateDDLWithOptions(schema.DDLOptions{Dialect: interBaseDDLDialectFor(req.Dialect)})
				if err != nil {
					return metadata.Detail{}, interBaseWrapDDLError(err)
				}
				acquired.Text = ddl
				acquired.Provenance = "interbase:view-ddl"
				return acquired, nil
			},
		}, nil
	default:
		// Table DDL needs constraint/index/trigger facts no bulk category
		// retains, and a faithful Load/Render split for it (I/O confined to
		// Load, dialect confined to Render) is future work. Function DDL is
		// permanently refused by the driver itself. Neither silently
		// triggers a full-catalog refetch.
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

// --- retained-fact -> driver value reconstruction --------------------------

func interBaseDDLDialectFor(dialect int) schema.DDLDialect {
	if dialect == 1 {
		return schema.Dialect1
	}
	return schema.Dialect3
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
		Name:          desc.Name,
		ProcedureName: procedureName,
		Number:        sql.NullInt64{Int64: int64(desc.Position), Valid: true},
		Direction:     direction,
		ParameterType: parameterType,
		Nullable:      desc.Nullable,
	}
	if desc.Facts != nil {
		parameter.Domain = interBaseSchemaDomainFromFacts(desc.Facts)
		parameter.FieldSource = desc.Facts.DomainName
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
