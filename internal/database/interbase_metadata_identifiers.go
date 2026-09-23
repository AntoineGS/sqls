package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"interbase-go/schema"
)

const interBaseMetadataIdentifierWidthQuery = `
SELECT MAX(f.RDB$FIELD_LENGTH)
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME LIKE 'RDB$%'
  AND f.RDB$FIELD_TYPE IN (14, 37)`

// interBaseMetadataIdentifierWidth discovers the maximum declared byte capacity
// of catalog identifier fields for the job's snapshot. A missing width is an
// error: guessing a smaller capacity can silently merge distinct identifiers.
func interBaseMetadataIdentifierWidth(ctx context.Context, q schema.Queryer) (result int, err error) {
	if q == nil {
		return 0, errors.New("interbase: identifier width query source is nil")
	}
	rows, err := q.QueryContext(ctx, interBaseMetadataIdentifierWidthQuery)
	if err != nil {
		return 0, fmt.Errorf("interbase: discover identifier width: %w", err)
	}
	if rows == nil {
		return 0, errors.New("interbase: identifier width query returned no rows handle")
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, rows.Close())
		}
	}()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("interbase: read identifier width: %w", err)
		}
		return 0, errors.New("interbase: identifier width query returned no row")
	}
	var width sql.NullInt64
	if err := rows.Scan(&width); err != nil {
		return 0, fmt.Errorf("interbase: scan identifier width: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("interbase: read identifier width: %w", err)
	}
	closeErr := rows.Close()
	closed = true
	if closeErr != nil {
		return 0, fmt.Errorf("interbase: close identifier width rows: %w", closeErr)
	}
	if !width.Valid || width.Int64 <= 0 || uint64(width.Int64) > uint64(maxInt()) {
		return 0, fmt.Errorf("interbase: invalid identifier width %v", width)
	}
	return int(width.Int64), nil
}

func maxInt() int { return int(^uint(0) >> 1) }

// interBaseMetadataIdentifier projects a catalog identifier through VARCHAR at
// the catalog-derived width. ref is an internal SQL column expression, never
// caller-provided text; only the validated integer is formatted.
func interBaseMetadataIdentifier(ref string, width int) string {
	if width <= 0 {
		return ""
	}
	return fmt.Sprintf("CAST(%s AS VARCHAR(%d))", ref, width)
}
