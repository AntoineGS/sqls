package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

const watchedNameBatch = 50

var watchedWidthDiscovery = regexp.MustCompile(`(?is)^\s*SELECT CAST\(rf\.RDB\$RELATION_NAME AS VARCHAR\([0-9]+\)\),\s*CAST\(rf\.RDB\$FIELD_NAME AS VARCHAR\([0-9]+\)\),\s*f\.RDB\$FIELD_LENGTH\s+FROM RDB\$RELATION_FIELDS rf\s+JOIN RDB\$FIELDS f ON rf\.RDB\$FIELD_SOURCE = f\.RDB\$FIELD_NAME\s*$`)

func (db *InterBaseDBRepository) ReadWatchedDelta(ctx context.Context, base *catalog.Cache, previous, current *schema.CatalogFingerprint) (delta metadata.ObjectDelta, err error) {
	start := time.Now()
	defer func() {
		if err == nil {
			delta.Metrics.Duration = time.Since(start)
			delta.Metrics.DurationKnown = true
		}
	}()
	if err := ctx.Err(); err != nil {
		return metadata.ObjectDelta{}, err
	}
	selection, err := mapWatchedChanges(ctx, base, previous, current)
	if err != nil {
		return metadata.ObjectDelta{}, err
	}
	return db.ReadWatchedChanges(ctx, base, selection)
}

// watchedQueryer scopes the existing, validated category projections. The
// closed category/from-table whitelist fails closed if a projection changes;
// only the existing scalar identifier-width discovery bypasses name filtering.
type watchedQueryer struct {
	queryer schema.Queryer
	kind    catalog.MetadataKind
	owners  []catalog.ObjectID
}

func (q watchedQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	u := strings.ToUpper(query)
	if len(args) == 0 && watchedWidthDiscovery.MatchString(query) {
		return q.queryer.QueryContext(ctx, query)
	}
	if strings.TrimSpace(u) == strings.ToUpper(strings.TrimSpace(`SELECT f.RDB$FIELD_LENGTH
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME = ?
  AND rf.RDB$FIELD_NAME = ?`)) && len(args) == 2 {
		return q.queryer.QueryContext(ctx, query, args...)
	}
	if len(args) != 0 || len(q.owners) == 0 || len(q.owners) > watchedNameBatch {
		return nil, metadata.ErrWatchIncomplete
	}
	var refs []string
	switch q.kind {
	case catalog.MetadataRelations, catalog.MetadataViews, catalog.MetadataSystemRelations:
		if strings.Contains(u, "FROM RDB$RELATIONS R\n") {
			refs = []string{"r.RDB$RELATION_NAME"}
		}
		if q.kind == catalog.MetadataViews && strings.Contains(u, "FROM RDB$RELATION_FIELDS RF\n") {
			refs = []string{"rf.RDB$RELATION_NAME"}
		}
	case catalog.MetadataColumnsCurrent:
		if strings.Contains(u, "FROM RDB$RELATION_FIELDS RF\n") {
			refs = []string{"rf.RDB$RELATION_NAME"}
		}
	case catalog.MetadataPrimaryKeys:
		if strings.Contains(u, "FROM RDB$RELATION_CONSTRAINTS PK\n") {
			refs = []string{"pk.RDB$RELATION_NAME"}
		}
	case catalog.MetadataForeignKeys:
		if strings.Contains(u, "FROM RDB$RELATION_CONSTRAINTS FK\n") {
			refs = []string{"fk.RDB$RELATION_NAME", "pk.RDB$RELATION_NAME"}
		}
	case catalog.MetadataProcedures:
		if strings.Contains(u, "FROM RDB$PROCEDURES P\n") || strings.Contains(u, "FROM RDB$PROCEDURE_PARAMETERS PP\n") {
			refs = []string{"p.RDB$PROCEDURE_NAME"}
		}
	case catalog.MetadataFunctions:
		if strings.Contains(u, "FROM RDB$FUNCTIONS F\n") || strings.Contains(u, "FROM RDB$FUNCTION_ARGUMENTS A\n") {
			refs = []string{"f.RDB$FUNCTION_NAME"}
		}
	case catalog.MetadataDomains:
		if strings.Contains(u, "FROM RDB$FIELDS F\n") {
			refs = []string{"f.RDB$FIELD_NAME"}
		}
	case catalog.MetadataGenerators:
		if strings.Contains(u, "FROM RDB$GENERATORS G") {
			refs = []string{"g.RDB$GENERATOR_NAME"}
		}
	case catalog.MetadataExceptions:
		if strings.Contains(u, "FROM RDB$EXCEPTIONS E") {
			refs = []string{"e.RDB$EXCEPTION_NAME"}
		}
	case catalog.MetadataIndexes:
		if strings.Contains(u, "FROM RDB$INDICES I\n") || strings.Contains(u, "FROM RDB$INDEX_SEGMENTS S\n") {
			refs = []string{"i.RDB$INDEX_NAME", "i.RDB$RELATION_NAME"}
		}
	case catalog.MetadataTriggers:
		if strings.Contains(u, "FROM RDB$TRIGGERS T") {
			refs = []string{"t.RDB$TRIGGER_NAME", "t.RDB$RELATION_NAME"}
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: unsupported scoped projection", metadata.ErrWatchIncomplete)
	}
	var predicates []string
	for i, ref := range refs {
		var names []any
		for _, owner := range q.owners {
			if q.kind == catalog.MetadataIndexes && ((i == 0) != (owner.Kind == catalog.ObjectKindIndex)) {
				continue
			}
			if q.kind == catalog.MetadataTriggers && ((i == 0) != (owner.Kind == catalog.ObjectKindTrigger)) {
				continue
			}
			names = append(names, owner.Name)
		}
		if len(names) == 0 {
			continue
		}
		predicates = append(predicates, ref+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+")")
		args = append(args, names...)
	}
	if len(predicates) == 0 {
		return nil, metadata.ErrWatchIncomplete
	}
	predicate := "(" + strings.Join(predicates, " OR ") + ")"
	if index := strings.Index(u, "WHERE "); index >= 0 {
		query = query[:index] + "WHERE " + predicate + " AND (" + query[index+6:]
		u = strings.ToUpper(query)
		if order := strings.Index(u, "ORDER BY "); order >= 0 {
			query = query[:order] + ")\n" + query[order:]
		} else {
			query += ")"
		}
	} else if index := strings.Index(u, "ORDER BY "); index >= 0 {
		query = query[:index] + "WHERE " + predicate + "\n" + query[index:]
	} else {
		query += "\nWHERE " + predicate
	}
	return q.queryer.QueryContext(ctx, query, args...)
}

func (db *InterBaseDBRepository) readWatchedCategory(ctx context.Context, q schema.Queryer, width int, kind catalog.MetadataKind, names []string) (MetadataPatch, error) {
	ids := make([]catalog.ObjectID, len(names))
	for i, name := range names {
		ids[i] = catalog.ObjectID{Name: name, Kind: catalog.ObjectKind(kind)}
	}
	return db.readWatchedCategoryOwners(ctx, q, width, kind, ids)
}
func (db *InterBaseDBRepository) readWatchedCategoryOwners(ctx context.Context, q schema.Queryer, width int, kind catalog.MetadataKind, owners []catalog.ObjectID) (MetadataPatch, error) {
	scoped := watchedQueryer{queryer: q, kind: kind, owners: owners}
	switch kind {
	case catalog.MetadataRelations:
		return db.readMetadataRelations(ctx, scoped, width)
	case catalog.MetadataColumnsCurrent:
		return db.readMetadataColumns(ctx, scoped, width)
	case catalog.MetadataPrimaryKeys:
		return db.readWatchedPrimaryKeys(ctx, scoped, width)
	case catalog.MetadataViews:
		return db.readMetadataViews(ctx, scoped, width)
	case catalog.MetadataProcedures:
		return db.readMetadataProcedures(ctx, scoped, width)
	case catalog.MetadataIndexes:
		return db.readMetadataIndexes(ctx, scoped, width)
	case catalog.MetadataForeignKeys:
		return db.readWatchedForeignKeys(ctx, scoped, width, map[watchedFKIdentity]bool{})
	case catalog.MetadataFunctions:
		return db.readMetadataFunctions(ctx, scoped, width)
	default:
		bound := &InterBaseDBRepository{Conn: db.Conn, SQLDialect: db.SQLDialect, SourceSQLDialect: db.SourceSQLDialect, DatabaseName: db.DatabaseName, snapshot: &interBaseCatalogSnapshot{catalog: schema.New(scoped)}}
		return bound.readMetadataAccessor(ctx, kind)
	}
}

func watchedHeaderPresent(kind catalog.MetadataKind, c *catalog.Cache, id catalog.ObjectID) bool {
	if kind == catalog.MetadataRelations {
		for _, name := range c.SchemaTables[""] {
			if name == id.Name {
				return true
			}
		}
		return false
	}
	if c.Catalog == nil {
		return false
	}
	switch kind {
	case catalog.MetadataViews:
		return c.Catalog.ViewsByID[id.Key()] != nil
	case catalog.MetadataProcedures:
		return c.Catalog.ProceduresByID[id.Key()] != nil
	case catalog.MetadataFunctions:
		return c.Catalog.FunctionsByID[id.Key()] != nil
	case catalog.MetadataDomains:
		return c.Catalog.DomainsByID[id.Key()] != nil
	case catalog.MetadataTriggers:
		return c.Catalog.TriggersByID[id.Key()] != nil
	case catalog.MetadataIndexes:
		return c.Catalog.IndexesByID[id.Key()] != nil
	case catalog.MetadataGenerators:
		return c.Catalog.GeneratorsByID[id.Key()] != nil
	case catalog.MetadataExceptions:
		return c.Catalog.ExceptionsByID[id.Key()] != nil
	case catalog.MetadataSystemRelations:
		return c.Catalog.SystemRelationsByID[id.Key()] != nil
	}
	return true
}

func (db *InterBaseDBRepository) ReadWatchedChanges(ctx context.Context, base *catalog.Cache, selection watchedSelection) (delta metadata.ObjectDelta, err error) {
	start := time.Now()
	if err = ctx.Err(); err != nil {
		return metadata.ObjectDelta{}, err
	}
	if selection.FullReason != "" {
		return metadata.ObjectDelta{FullReason: selection.FullReason}, nil
	}
	if base == nil || base.Catalog == nil || base.Dependencies == nil || !base.MetadataReady(catalog.MetadataDependencies) || base.ColumnsByID == nil || base.PrimaryKeysByID == nil {
		return metadata.ObjectDelta{FullReason: "mapping"}, nil
	}
	for kind := range selection.Owners {
		if !base.MetadataReady(kind) || !watchedBaseCategoryAvailable(base, kind) {
			return metadata.ObjectDelta{FullReason: "mapping"}, nil
		}
	}
	delta = metadata.ObjectDelta{Owners: selection.Owners, Deleted: selection.Deleted, DependencyOwners: selection.DependencyOwners, DependencyEdges: selection.dependencyEdges, CheckNames: selection.checkNames, Affected: selection.Affected, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{}}
	var kinds []catalog.MetadataKind
	for kind := range selection.Owners {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, kind := range kinds {
		ids := selection.Owners[kind]
		var complete MetadataPatch
		seenFK := map[watchedFKIdentity]bool{}
		for offset := 0; offset < len(ids); offset += watchedNameBatch {
			end := offset + watchedNameBatch
			if end > len(ids) {
				end = len(ids)
			}
			patch, readErr := db.runWatchedRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				if kind == catalog.MetadataForeignKeys {
					return db.readWatchedForeignKeys(ctx, watchedQueryer{queryer: q, kind: kind, owners: ids[offset:end]}, width, seenFK)
				}
				return db.readWatchedCategoryOwners(ctx, q, width, kind, ids[offset:end])
			})
			if readErr != nil {
				return metadata.ObjectDelta{}, errors.Join(metadata.ErrWatchIncomplete, readErr)
			}
			if err := validateWatchedOrdering(kind, patch.Cache); err != nil {
				return metadata.ObjectDelta{}, err
			}
			for _, id := range ids[offset:end] {
				if selection.expected[kind][id] && !watchedHeaderPresent(kind, patch.Cache, id) {
					return metadata.ObjectDelta{}, fmt.Errorf("%w: eligible scoped header missing", metadata.ErrWatchIncomplete)
				}
			}
			appendWatchedPatch(&complete, patch)
		}
		delta.Patches[kind] = complete
		for id := range selection.expected[kind] {
			if !watchedHeaderPresent(kind, complete.Cache, id) {
				return metadata.ObjectDelta{}, fmt.Errorf("%w: candidate group header missing", metadata.ErrWatchIncomplete)
			}
		}
		for id, count := range selection.expectedChildren[kind] {
			actual := 0
			switch kind {
			case catalog.MetadataProcedures:
				if procedure := complete.Cache.Catalog.ProceduresByID[id.Key()]; procedure != nil {
					actual = len(procedure.InputParameters) + len(procedure.OutputParameters)
				}
			case catalog.MetadataFunctions:
				if function := complete.Cache.Catalog.FunctionsByID[id.Key()]; function != nil {
					actual = len(function.Arguments)
				}
			}
			if actual != count {
				return metadata.ObjectDelta{}, fmt.Errorf("%w: candidate signature row coverage", metadata.ErrWatchIncomplete)
			}
		}
		delta.Metrics.Queries += int64(complete.Queries)
	}
	delta.Metrics.Duration = time.Since(start)
	delta.Metrics.DurationKnown, delta.Metrics.QueriesKnown = true, true
	return delta, nil
}

func watchedBaseCategoryAvailable(base *catalog.Cache, kind catalog.MetadataKind) bool {
	c := base.Catalog
	switch kind {
	case catalog.MetadataRelations:
		return base.SchemaTables != nil
	case catalog.MetadataColumnsCurrent:
		return base.ColumnsByID != nil && base.ColumnsWithParent != nil
	case catalog.MetadataPrimaryKeys:
		return base.PrimaryKeysByID != nil && base.PrimaryKeyColumns != nil
	case catalog.MetadataForeignKeys:
		return base.ForeignKeys != nil
	case catalog.MetadataViews:
		return c.ViewsByID != nil
	case catalog.MetadataProcedures:
		return c.ProceduresByID != nil
	case catalog.MetadataFunctions:
		return c.FunctionsByID != nil
	case catalog.MetadataIndexes:
		return c.IndexesByID != nil
	case catalog.MetadataTriggers:
		return c.TriggersByID != nil
	case catalog.MetadataDomains:
		return c.DomainsByID != nil
	case catalog.MetadataGenerators:
		return c.GeneratorsByID != nil
	case catalog.MetadataExceptions:
		return c.ExceptionsByID != nil
	case catalog.MetadataSystemRelations:
		return c.SystemRelationsByID != nil
	}
	return false
}

func (db *InterBaseDBRepository) readWatchedPrimaryKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	query := interBaseBulkPrimaryKeyFieldsQueryForWidth(width)
	query = strings.Replace(query, "\nFROM RDB$RELATION_CONSTRAINTS pk", ", s.RDB$FIELD_POSITION, pi.RDB$SEGMENT_COUNT\nFROM RDB$RELATION_CONSTRAINTS pk", 1)
	query = strings.Replace(query, "\nJOIN RDB$INDICES pi", "\nLEFT JOIN RDB$INDICES pi", 1)
	query = strings.Replace(query, "\nJOIN RDB$INDEX_SEGMENTS s", "\nLEFT JOIN RDB$INDEX_SEGMENTS s", 1)
	exact := map[string][]string{}
	folded := map[string]map[string]struct{}{}
	counts := map[string]int64{}
	err := interBaseBulkQuery(ctx, q, "watched primary key fields", query, func(rows *sql.Rows) error {
		var relation, column sql.NullString
		var position, count sql.NullInt64
		if err := rows.Scan(&relation, &column, &position, &count); err != nil {
			return err
		}
		r, err := interBaseRequiredName(relation, "watched PK relation")
		if err != nil {
			return err
		}
		c, err := interBaseRequiredName(column, "watched PK column")
		if err != nil {
			return err
		}
		id := interBaseByIDKey(ObjectKindTable, r)
		if !position.Valid || position.Int64 != int64(len(exact[id])) || !count.Valid || count.Int64 <= 0 {
			return metadata.ErrWatchIncomplete
		}
		if previous, ok := counts[id]; ok && previous != count.Int64 {
			return metadata.ErrWatchIncomplete
		}
		counts[id] = count.Int64
		exact[id] = append(exact[id], c)
		key := columnDatabaseKey("", r)
		if folded[key] == nil {
			folded[key] = map[string]struct{}{}
		}
		folded[key][c] = struct{}{}
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	for id, count := range counts {
		if int64(len(exact[id])) != count {
			return MetadataPatch{}, metadata.ErrWatchIncomplete
		}
	}
	return MetadataPatch{Cache: &catalog.Cache{PrimaryKeysByID: exact, PrimaryKeyColumns: folded}, Count: len(folded)}, nil
}

func validateWatchedOrdering(kind catalog.MetadataKind, cache *catalog.Cache) error {
	if cache == nil {
		return metadata.ErrWatchIncomplete
	}
	if cache.Catalog == nil {
		return nil
	}
	if kind == catalog.MetadataFunctions {
		for _, function := range cache.Catalog.FunctionsByID {
			last := int64(-1)
			for _, argument := range function.Arguments {
				if argument == nil || !argument.Position.Valid || argument.Position.Int64 <= last {
					return metadata.ErrWatchIncomplete
				}
				last = argument.Position.Int64
			}
		}
	}
	if kind == catalog.MetadataProcedures {
		for _, procedure := range cache.Catalog.ProceduresByID {
			for _, parameters := range [][]*catalog.ProcedureParameterDesc{procedure.InputParameters, procedure.OutputParameters} {
				last := -1
				for _, parameter := range parameters {
					if parameter == nil || parameter.Position <= last {
						return metadata.ErrWatchIncomplete
					}
					last = parameter.Position
				}
			}
		}
	}
	return nil
}

type watchedFKIdentity struct{ relation, constraint string }

// Keep the ordinary loader's exact projection/converter, extending only the
// watched projection with position/count facts so corrupt keys fail instead of
// silently disappearing. Constraint identity, not rendered field pairs, dedups
// keys that are returned through both endpoints in different name batches.
func (db *InterBaseDBRepository) readWatchedForeignKeys(ctx context.Context, q schema.Queryer, width int, seen map[watchedFKIdentity]bool) (MetadataPatch, error) {
	query := interBaseBulkForeignKeyFieldsQueryForWidth(width)
	query = strings.Replace(query, "\nFROM RDB$RELATION_CONSTRAINTS fk", ", fs.RDB$FIELD_POSITION, fi.RDB$SEGMENT_COUNT\nFROM RDB$RELATION_CONSTRAINTS fk", 1)
	for _, join := range []string{"RDB$INDICES fi", "RDB$REF_CONSTRAINTS rc", "RDB$RELATION_CONSTRAINTS pk", "RDB$INDICES pi", "RDB$INDEX_SEGMENTS fs"} {
		query = strings.Replace(query, "\nJOIN "+join, "\nLEFT JOIN "+join, 1)
	}
	mappings := map[watchedFKIdentity]*interBaseForeignKeyMapping{}
	counts := map[watchedFKIdentity]int64{}
	var order []watchedFKIdentity
	err := interBaseBulkQuery(ctx, q, "watched foreign key fields", query, func(rows *sql.Rows) error {
		var constraint, relation, field, referenced, referencedField sql.NullString
		var position, count sql.NullInt64
		if err := rows.Scan(&constraint, &relation, &field, &referenced, &referencedField, &position, &count); err != nil {
			return err
		}
		values := []sql.NullString{constraint, relation, field, referenced, referencedField}
		names := make([]string, len(values))
		for i, v := range values {
			name, err := interBaseRequiredName(v, "watched FK identity")
			if err != nil {
				return err
			}
			names[i] = name
		}
		id := watchedFKIdentity{relation: names[1], constraint: names[0]}
		mapping := mappings[id]
		if mapping == nil {
			mapping = &interBaseForeignKeyMapping{relationName: names[1], referencedRelationName: names[3]}
			mappings[id] = mapping
			order = append(order, id)
			if !count.Valid || count.Int64 <= 0 {
				return metadata.ErrWatchIncomplete
			}
			counts[id] = count.Int64
		}
		if !position.Valid || position.Int64 != int64(len(mapping.columns)) || !count.Valid || count.Int64 != counts[id] || mapping.referencedRelationName != names[3] {
			return metadata.ErrWatchIncomplete
		}
		mapping.columns = append(mapping.columns, names[2])
		mapping.referencedColumns = append(mapping.referencedColumns, names[4])
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	keys := map[string]map[string][]*catalog.ForeignKey{}
	n := 0
	for _, id := range order {
		mapping := mappings[id]
		if int64(len(mapping.columns)) != counts[id] {
			return MetadataPatch{}, metadata.ErrWatchIncomplete
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		n++
		for _, key := range interBaseForeignKeys([]interBaseForeignKeyMapping{*mapping}) {
			pair := (*key)[0]
			left, right := columnDatabaseKey("", pair[0].Table), columnDatabaseKey("", pair[1].Table)
			if keys[left] == nil {
				keys[left] = map[string][]*catalog.ForeignKey{}
			}
			if keys[right] == nil {
				keys[right] = map[string][]*catalog.ForeignKey{}
			}
			keys[left][right] = append(keys[left][right], key)
			keys[right][left] = append(keys[right][left], key)
		}
	}
	return MetadataPatch{Cache: &catalog.Cache{ForeignKeys: keys}, Count: n}, nil
}

func appendWatchMap[K comparable, V any](destination *map[K]V, source map[K]V) {
	if source == nil {
		return
	}
	if *destination == nil {
		*destination = make(map[K]V)
	}
	for k, v := range source {
		(*destination)[k] = v
	}
}
func appendWatchLists[K comparable, V any](destination *map[K][]V, source map[K][]V) {
	if source == nil {
		return
	}
	if *destination == nil {
		*destination = make(map[K][]V)
	}
	for k, v := range source {
		(*destination)[k] = append((*destination)[k], v...)
	}
}
func appendWatchedPatch(dst *MetadataPatch, src MetadataPatch) {
	if dst.Cache == nil {
		dst.Cache = &catalog.Cache{}
	}
	dst.Count += src.Count
	dst.Queries += src.Queries
	dst.QueriesKnown = true
	a, b := dst.Cache, src.Cache
	appendWatchLists(&a.SchemaTables, b.SchemaTables)
	appendWatchLists(&a.ColumnsWithParent, b.ColumnsWithParent)
	appendWatchLists(&a.ColumnsByID, b.ColumnsByID)
	appendWatchMap(&a.PrimaryKeysByID, b.PrimaryKeysByID)
	if b.PrimaryKeyColumns != nil {
		if a.PrimaryKeyColumns == nil {
			a.PrimaryKeyColumns = map[string]map[string]struct{}{}
		}
		for key, values := range b.PrimaryKeyColumns {
			merged := a.PrimaryKeyColumns[key]
			appendWatchMap(&merged, values)
			a.PrimaryKeyColumns[key] = merged
		}
	}
	if b.ForeignKeys != nil {
		if a.ForeignKeys == nil {
			a.ForeignKeys = map[string]map[string][]*catalog.ForeignKey{}
		}
		for key, values := range b.ForeignKeys {
			merged := a.ForeignKeys[key]
			appendWatchLists(&merged, values)
			a.ForeignKeys[key] = merged
		}
	}
	if b.Catalog == nil {
		return
	}
	if a.Catalog == nil {
		a.Catalog = &catalog.CatalogCache{}
	}
	c, d := a.Catalog, b.Catalog
	appendWatchMap(&c.Views, d.Views)
	appendWatchMap(&c.ViewsByID, d.ViewsByID)
	appendWatchMap(&c.Procedures, d.Procedures)
	appendWatchMap(&c.ProceduresByID, d.ProceduresByID)
	appendWatchMap(&c.Functions, d.Functions)
	appendWatchMap(&c.FunctionsByID, d.FunctionsByID)
	appendWatchMap(&c.Indexes, d.Indexes)
	appendWatchMap(&c.IndexesByID, d.IndexesByID)
	appendWatchMap(&c.Triggers, d.Triggers)
	appendWatchMap(&c.TriggersByID, d.TriggersByID)
	appendWatchMap(&c.Domains, d.Domains)
	appendWatchMap(&c.DomainsByID, d.DomainsByID)
	appendWatchMap(&c.Generators, d.Generators)
	appendWatchMap(&c.GeneratorsByID, d.GeneratorsByID)
	appendWatchMap(&c.ExceptionsByID, d.ExceptionsByID)
	appendWatchMap(&c.SystemRelationsByID, d.SystemRelationsByID)
}
