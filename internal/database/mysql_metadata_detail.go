package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// MetadataDetailPlan adapts MySQL detail requests onto the repository's
// schema-scoped cache. DDL is the only detail that performs additional I/O;
// it uses SHOW CREATE in a short read-only transaction.
func (db *MySQLMetadataRepository) MetadataDetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if db == nil || db.Schema == "" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	if req.Consistency == metadata.TransactionLocal || req.Consistency == metadata.ConsistentSnapshot {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	switch req.Facet {
	case "columns":
		return db.mysqlDetailPlanColumns(req)
	case "description":
		return db.mysqlDetailPlanDescription(req)
	case "source":
		return db.mysqlDetailPlanSource(req)
	case "ddl":
		return db.mysqlDetailPlanDDL(req)
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

func mysqlDetailCoverage(object catalog.ObjectID, facet string) string {
	return object.Key() + "\x00" + facet
}

func (db *MySQLMetadataRepository) mysqlDetailPlanColumns(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case catalog.ObjectKindTable:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataViews},
			Coverage: mysqlDetailCoverage(req.Object, "columns"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, err := db.mysqlResolveTableName(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				columns, ok := cache.ColumnDescsByID(name)
				if !ok {
					return metadata.Detail{}, ErrObjectNotFound
				}
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindTable, name), Columns: columns}, nil
			},
		}, nil
	case catalog.ObjectKindView:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataViews},
			Coverage: mysqlDetailCoverage(req.Object, "columns"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, view, err := db.mysqlResolveView(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindView, name), Columns: view.Columns}, nil
			},
		}, nil
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

func (db *MySQLMetadataRepository) mysqlDetailPlanDescription(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case catalog.ObjectKindTable:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataViews},
			Coverage: mysqlDetailCoverage(req.Object, "description"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, err := db.mysqlResolveTableName(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				columns, ok := cache.ColumnDescsByID(name)
				if !ok {
					return metadata.Detail{}, ErrObjectNotFound
				}
				description := mysqlMetadataDescription(catalog.ObjectKindTable, name, columns)
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindTable, name), Columns: columns, Description: description}, nil
			},
		}, nil
	case catalog.ObjectKindView:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataViews},
			Coverage: mysqlDetailCoverage(req.Object, "description"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, view, err := db.mysqlResolveView(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				description := mysqlMetadataDescription(catalog.ObjectKindView, name, view.Columns)
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindView, name), Columns: view.Columns, Description: description}, nil
			},
		}, nil
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

func (db *MySQLMetadataRepository) mysqlDetailPlanSource(req metadata.Request) (metadata.DetailPlan, error) {
	switch req.Object.Kind {
	case catalog.ObjectKindView:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataViews},
			Coverage: mysqlDetailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, view, err := db.mysqlResolveView(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindView, name), Text: view.ViewSource.String, Provenance: "mysql:view-source"}, nil
			},
		}, nil
	case catalog.ObjectKindProcedure:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataProcedures},
			Coverage: mysqlDetailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, procedure, err := db.mysqlResolveProcedure(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindProcedure, name), Text: procedure.Source.String, Provenance: "mysql:procedure-source"}, nil
			},
		}, nil
	case catalog.ObjectKindTrigger:
		return metadata.DetailPlan{
			Requires: []catalog.MetadataKind{catalog.MetadataTriggers},
			Coverage: mysqlDetailCoverage(req.Object, "source"),
			Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
				schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
				if err != nil {
					return metadata.Detail{}, err
				}
				name, trigger, err := db.mysqlResolveTrigger(cache, schema, req.Object.Name)
				if err != nil {
					return metadata.Detail{}, err
				}
				return metadata.Detail{Object: mysqlDetailObject(schema, catalog.ObjectKindTrigger, name), Text: trigger.Source.String, Provenance: "mysql:trigger-source"}, nil
			},
		}, nil
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
}

func (db *MySQLMetadataRepository) mysqlDetailPlanDDL(req metadata.Request) (metadata.DetailPlan, error) {
	var requires []catalog.MetadataKind
	switch req.Object.Kind {
	case catalog.ObjectKindTable:
		requires = []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataViews}
	case catalog.ObjectKindView:
		requires = []catalog.MetadataKind{catalog.MetadataViews}
	case catalog.ObjectKindProcedure:
		requires = []catalog.MetadataKind{catalog.MetadataProcedures}
	case catalog.ObjectKindFunction:
		requires = []catalog.MetadataKind{catalog.MetadataFunctions}
	case catalog.ObjectKindTrigger:
		requires = []catalog.MetadataKind{catalog.MetadataTriggers}
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{
		Requires: requires,
		Coverage: mysqlDetailCoverage(req.Object, "ddl"),
		Load: func(ctx context.Context, cache *catalog.Cache) (metadata.Detail, error) {
			schema, err := db.mysqlCanonicalDetailSchema(cache, req.Object.Schema)
			if err != nil {
				return metadata.Detail{}, err
			}
			name, err := db.mysqlResolveObjectName(cache, schema, req.Object.Kind, req.Object.Name)
			if err != nil {
				return metadata.Detail{}, err
			}
			statement, ddlLabel := mysqlShowCreateStatement(req.Object.Kind, schema, name)
			var ddl string
			err = db.withMetadataDetailRead(ctx, func(tx *sql.Tx) error {
				var readErr error
				ddl, readErr = mysqlReadShowCreate(ctx, tx, statement, ddlLabel)
				return readErr
			})
			if err != nil {
				return metadata.Detail{}, err
			}
			return metadata.Detail{
				Object:     mysqlDetailObject(schema, req.Object.Kind, name),
				Text:       ddl,
				Provenance: "mysql:" + string(req.Object.Kind) + "-ddl",
			}, nil
		},
	}, nil
}

func (db *MySQLMetadataRepository) mysqlCanonicalDetailSchema(cache *catalog.Cache, requestSchema string) (string, error) {
	if cache == nil || cache.DefaultSchema == "" {
		return "", errors.New("mysql: canonical detail schema is unavailable")
	}
	// The configured target may use a different case from the server's
	// canonical schema spelling, so compare those two case-insensitively.
	if db == nil || db.Schema == "" || !strings.EqualFold(db.Schema, cache.DefaultSchema) {
		return "", metadata.ErrUnsupported
	}
	if requestSchema != "" && requestSchema != cache.DefaultSchema {
		return "", metadata.ErrUnsupported
	}
	return cache.DefaultSchema, nil
}

func (db *MySQLMetadataRepository) mysqlResolveObjectName(cache *catalog.Cache, schema string, kind catalog.ObjectKind, want string) (string, error) {
	if cache == nil {
		return "", ErrObjectNotFound
	}
	switch kind {
	case catalog.ObjectKindTable:
		return mysqlResolveName(kind, mysqlTableNames(cache, schema), want)
	case catalog.ObjectKindView:
		return mysqlResolveDescriptorName(kind, want, schema, cache.Catalog, func(c *catalog.CatalogCache) map[string]*catalog.ViewDesc { return c.ViewsByID }, func(desc *catalog.ViewDesc) (string, string) { return desc.Schema, desc.Name })
	case catalog.ObjectKindProcedure:
		return mysqlResolveDescriptorName(kind, want, schema, cache.Catalog, func(c *catalog.CatalogCache) map[string]*catalog.ProcedureDesc { return c.ProceduresByID }, func(desc *catalog.ProcedureDesc) (string, string) { return desc.Schema, desc.Name })
	case catalog.ObjectKindFunction:
		return mysqlResolveDescriptorName(kind, want, schema, cache.Catalog, func(c *catalog.CatalogCache) map[string]*catalog.FunctionDesc { return c.FunctionsByID }, func(desc *catalog.FunctionDesc) (string, string) { return desc.Schema, desc.Name })
	case catalog.ObjectKindTrigger:
		return mysqlResolveDescriptorName(kind, want, schema, cache.Catalog, func(c *catalog.CatalogCache) map[string]*catalog.TriggerDesc { return c.TriggersByID }, func(desc *catalog.TriggerDesc) (string, string) { return desc.Schema, desc.Name })
	default:
		return "", metadata.ErrUnsupported
	}
}

// mysqlTableNames excludes views because SchemaTables deliberately contains
// both relation kinds for browsing/completion. The view descriptors provide
// the exact classification for this cache generation.
func mysqlTableNames(cache *catalog.Cache, schema string) []string {
	if cache == nil || cache.Catalog == nil || cache.Catalog.ViewsByID == nil {
		return nil
	}
	views := make(map[string]struct{})
	for _, view := range cache.Catalog.ViewsByID {
		if view != nil && view.Schema == schema {
			views[view.Name] = struct{}{}
		}
	}
	names := cache.SchemaTables[strings.ToUpper(schema)]
	tables := make([]string, 0, len(names))
	for _, name := range names {
		if _, isView := views[name]; !isView {
			tables = append(tables, name)
		}
	}
	return tables
}

func (db *MySQLMetadataRepository) mysqlResolveTableName(cache *catalog.Cache, schema, want string) (string, error) {
	return db.mysqlResolveObjectName(cache, schema, catalog.ObjectKindTable, want)
}

func (db *MySQLMetadataRepository) mysqlResolveView(cache *catalog.Cache, schema, want string) (string, *catalog.ViewDesc, error) {
	name, err := db.mysqlResolveObjectName(cache, schema, catalog.ObjectKindView, want)
	if err != nil {
		return "", nil, err
	}
	view := cache.Catalog.ViewsByID[(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: name}).Key()]
	return name, view, nil
}

func (db *MySQLMetadataRepository) mysqlResolveProcedure(cache *catalog.Cache, schema, want string) (string, *catalog.ProcedureDesc, error) {
	name, err := db.mysqlResolveObjectName(cache, schema, catalog.ObjectKindProcedure, want)
	if err != nil {
		return "", nil, err
	}
	procedure := cache.Catalog.ProceduresByID[(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: name}).Key()]
	return name, procedure, nil
}

func (db *MySQLMetadataRepository) mysqlResolveTrigger(cache *catalog.Cache, schema, want string) (string, *catalog.TriggerDesc, error) {
	name, err := db.mysqlResolveObjectName(cache, schema, catalog.ObjectKindTrigger, want)
	if err != nil {
		return "", nil, err
	}
	trigger := cache.Catalog.TriggersByID[(catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: name}).Key()]
	return name, trigger, nil
}

func mysqlResolveDescriptorName[T any](kind catalog.ObjectKind, want, schema string, cache *catalog.CatalogCache, descriptors func(*catalog.CatalogCache) map[string]*T, identity func(*T) (string, string)) (string, error) {
	if cache == nil {
		return "", ErrObjectNotFound
	}
	byID := descriptors(cache)
	names := make([]string, 0, len(byID))
	for _, descriptor := range byID {
		if descriptor == nil {
			continue
		}
		descriptorSchema, name := identity(descriptor)
		if descriptorSchema == schema && name != "" {
			names = append(names, name)
		}
	}
	return mysqlResolveName(kind, names, want)
}

func mysqlResolveName(kind catalog.ObjectKind, candidates []string, want string) (string, error) {
	var folded string
	matches := 0
	for _, candidate := range candidates {
		if candidate == want {
			return candidate, nil
		}
		if strings.EqualFold(candidate, want) {
			folded = candidate
			matches++
		}
	}
	switch matches {
	case 0:
		return "", ErrObjectNotFound
	case 1:
		return folded, nil
	default:
		return "", fmt.Errorf("mysql: %s name %q is ambiguous among %d matches: %w", kind, want, matches, catalog.ErrAmbiguous)
	}
}

func mysqlDetailObject(schema string, kind catalog.ObjectKind, name string) catalog.ObjectID {
	return catalog.ObjectID{Schema: schema, Kind: kind, Name: name}
}

func mysqlMetadataDescription(kind catalog.ObjectKind, name string, columns []*catalog.ColumnDesc) catalog.TableDescription {
	var body strings.Builder
	if kind == catalog.ObjectKindView {
		body.WriteString("CREATE VIEW ")
	} else {
		body.WriteString("CREATE TABLE ")
	}
	tableSpan := mysqlWriteDescriptionIdentifier(&body, name)
	body.WriteString(" (\n")
	descriptionColumns := make([]catalog.DescriptionColumn, 0, len(columns))
	for _, column := range columns {
		if column == nil {
			continue
		}
		if len(descriptionColumns) > 0 {
			body.WriteString(",\n")
		}
		body.WriteString("  ")
		span := mysqlWriteDescriptionIdentifier(&body, column.Name)
		descriptionColumns = append(descriptionColumns, catalog.DescriptionColumn{Name: column.Name, Span: span})
		if column.Type != "" {
			body.WriteByte(' ')
			body.WriteString(column.Type)
		}
		if column.Null == "NO" {
			body.WriteString(" NOT NULL")
		}
	}
	body.WriteString("\n)")
	return catalog.TableDescription{Body: body.String(), Table: tableSpan, Columns: descriptionColumns}
}

func mysqlWriteDescriptionIdentifier(body *strings.Builder, name string) catalog.DescriptionSpan {
	start := body.Len()
	body.WriteString(mysqlQuoteIdentifier(name))
	return catalog.DescriptionSpan{Start: start, End: body.Len()}
}

func mysqlQuoteIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func mysqlShowCreateStatement(kind catalog.ObjectKind, schema, name string) (string, string) {
	verb, label := "", ""
	switch kind {
	case catalog.ObjectKindTable:
		verb, label = "TABLE", "Create Table"
	case catalog.ObjectKindView:
		verb, label = "VIEW", "Create View"
	case catalog.ObjectKindProcedure:
		verb, label = "PROCEDURE", "Create Procedure"
	case catalog.ObjectKindFunction:
		verb, label = "FUNCTION", "Create Function"
	case catalog.ObjectKindTrigger:
		verb, label = "TRIGGER", "SQL Original Statement"
	}
	return "SHOW CREATE " + verb + " " + mysqlQuoteIdentifier(schema) + "." + mysqlQuoteIdentifier(name), label
}

func (db *MySQLMetadataRepository) withMetadataDetailRead(ctx context.Context, read func(*sql.Tx) error) (err error) {
	if db == nil || db.Conn == nil {
		return errors.New("mysql: metadata database is nil")
	}
	if ctx == nil {
		return errors.New("mysql: metadata context is nil")
	}
	if read == nil {
		return errors.New("mysql: metadata detail read is nil")
	}
	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("mysql: begin metadata detail read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("mysql: rollback metadata detail read: %w", rollbackErr))
			}
		}
	}()
	if err = read(tx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("mysql: commit metadata detail read: %w", err)
	}
	committed = true
	return nil
}

func mysqlReadShowCreate(ctx context.Context, tx *sql.Tx, statement, ddlLabel string) (ddl string, err error) {
	rows, err := tx.QueryContext(ctx, statement)
	if err != nil {
		return "", fmt.Errorf("mysql: %s: %w", statement, err)
	}
	if rows == nil {
		return "", fmt.Errorf("mysql: %s returned nil rows", statement)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mysql: close SHOW CREATE rows: %w", closeErr))
			ddl = ""
		}
	}()
	labels, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("mysql: read SHOW CREATE columns: %w", err)
	}
	ddlIndex := -1
	for i, label := range labels {
		if label == ddlLabel {
			ddlIndex = i
			break
		}
	}
	if ddlIndex < 0 {
		return "", fmt.Errorf("mysql: SHOW CREATE result has no %q column", ddlLabel)
	}
	values := make([]any, len(labels))
	dest := make([]any, len(labels))
	for i := range values {
		dest[i] = &values[i]
	}
	found := false
	multiple := false
	invalidDDL := false
	for rows.Next() {
		for i := range values {
			values[i] = nil
		}
		if err := rows.Scan(dest...); err != nil {
			return "", fmt.Errorf("mysql: scan SHOW CREATE result: %w", err)
		}
		if found {
			multiple = true
			continue
		}
		text, ok := mysqlStringValue(values[ddlIndex])
		if !ok {
			invalidDDL = true
			continue
		}
		ddl, found = text, true
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("mysql: iterate SHOW CREATE rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if multiple {
		return "", fmt.Errorf("mysql: %s returned more than one row", statement)
	}
	if invalidDDL {
		return "", fmt.Errorf("mysql: SHOW CREATE %s column is NULL or not text", ddlLabel)
	}
	if !found {
		return "", ErrObjectNotFound
	}
	return ddl, nil
}

func mysqlStringValue(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	default:
		return "", false
	}
}
