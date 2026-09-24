package database

import (
	"context"
	"database/sql"
	"fmt"

	"interbase-go/schema"
)

func (db *InterBaseDBRepository) readMetadataIndexes(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	indexes := make([]schema.Index, 0)
	byName := make(map[string]int)
	headerQuery := fmt.Sprintf(`
SELECT %s, %s, i.RDB$UNIQUE_FLAG, i.RDB$DESCRIPTION,
       i.RDB$INDEX_INACTIVE, i.RDB$EXPRESSION_SOURCE, %s,
       i.RDB$SEGMENT_COUNT
FROM RDB$INDICES i
LEFT JOIN RDB$RELATION_CONSTRAINTS rc ON rc.RDB$INDEX_NAME = i.RDB$INDEX_NAME
WHERE COALESCE(i.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY i.RDB$INDEX_NAME`,
		interBaseMetadataIdentifier("i.RDB$INDEX_NAME", width),
		interBaseMetadataIdentifier("i.RDB$RELATION_NAME", width),
		interBaseMetadataIdentifier("rc.RDB$CONSTRAINT_NAME", width))
	err := interBaseBulkQuery(ctx, q, "index headers", headerQuery, func(rows *sql.Rows) error {
		var nameValue, relationValue, constraintName sql.NullString
		var index schema.Index
		if err := rows.Scan(&nameValue, &relationValue, &index.UniqueFlag, &index.Description, &index.Inactive, &index.Expression, &constraintName, &index.SegmentCount); err != nil {
			return err
		}
		name, err := interBaseRequiredName(nameValue, "index name")
		if err != nil {
			return err
		}
		relation, err := interBaseRequiredName(relationValue, "index relation name")
		if err != nil {
			return err
		}
		index.Name, index.RelationName = name, relation
		index.ConstraintName = trimInterBaseCatalogName(constraintName)
		if _, exists := byName[name]; exists {
			// Catalog.Indexes can return repeated headers for repeated constraint
			// rows, but all normal indexes have one optional owner constraint.
			// Treat a duplicate identity as corrupt instead of merging children.
			return fmt.Errorf("duplicate index header %q", name)
		}
		byName[name] = len(indexes)
		indexes = append(indexes, index)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	segmentsQuery := fmt.Sprintf(`
SELECT %s, %s, s.RDB$FIELD_POSITION
FROM RDB$INDEX_SEGMENTS s
JOIN RDB$INDICES i ON i.RDB$INDEX_NAME = s.RDB$INDEX_NAME
WHERE COALESCE(i.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY s.RDB$INDEX_NAME, s.RDB$FIELD_POSITION`,
		interBaseMetadataIdentifier("s.RDB$INDEX_NAME", width),
		interBaseMetadataIdentifier("s.RDB$FIELD_NAME", width))
	err = interBaseBulkQuery(ctx, q, "index segments", segmentsQuery, func(rows *sql.Rows) error {
		var rawIndex, rawField sql.NullString
		var rawPosition sql.NullInt64
		if err := rows.Scan(&rawIndex, &rawField, &rawPosition); err != nil {
			return err
		}
		indexName, err := interBaseRequiredName(rawIndex, "index segment index name")
		if err != nil {
			return err
		}
		fieldName, err := interBaseRequiredName(rawField, "index segment field name")
		if err != nil {
			return err
		}
		indexPosition, ok := byName[indexName]
		if !ok {
			return fmt.Errorf("index segment has no matching index header: %q", indexName)
		}
		if !rawPosition.Valid || rawPosition.Int64 < 0 || rawPosition.Int64 > int64(maxInt()) {
			return fmt.Errorf("index %q has invalid segment position %v", indexName, rawPosition)
		}
		segments := &indexes[indexPosition].Segments
		position := int(rawPosition.Int64)
		if position != len(*segments) {
			return fmt.Errorf("index %q has missing or duplicate segment position %d", indexName, position)
		}
		*segments = append(*segments, schema.IndexSegment{IndexName: indexName, FieldName: fieldName, Position: rawPosition})
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	for i := range indexes {
		index := &indexes[i]
		if index.Expression.Valid && len(index.Segments) == 0 {
			continue
		}
		if index.SegmentCount.Valid && index.SegmentCount.Int64 >= 0 && int64(len(index.Segments)) != index.SegmentCount.Int64 {
			return MetadataPatch{}, fmt.Errorf("index %q has %d segments, catalog declares %d", index.Name, len(index.Segments), index.SegmentCount.Int64)
		}
	}
	descriptions := db.indexDescriptions(indexes)
	byDescription := make(map[string]*IndexDesc, len(descriptions))
	for _, description := range descriptions {
		byDescription[description.Name] = description
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Indexes: byDescription}}, Count: len(descriptions)}, nil
}
