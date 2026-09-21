package database

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
)

// ColumnMeta is the immutable execution snapshot of one result column. Fields
// the driver cannot establish stay invalid rather than defaulting to a value
// the catalog never reported.
type ColumnMeta struct {
	Name             string
	DatabaseTypeName string
	Nullable         sql.NullBool
	Length           sql.NullInt64
	Precision        sql.NullInt64
	Scale            sql.NullInt64
}

// QueryResult is a fully materialised result set, rendered to strings.
type QueryResult struct {
	Columns []ColumnMeta
	Rows    [][]string
	Notes   []string
	// Complete is false when scanning stopped early because of an error.
	Complete bool
}

// RenderOptions controls driver-sensitive cell formatting.
//
// The zero value reproduces the existing ScanRows output for every non-NULL
// cell within the display cap. It diverges in exactly two places, both of them
// cross-cutting rather than InterBase-only, and both deliberate:
//
//   - a cell longer than DefaultMaxCellRunes is truncated, which ScanRows does
//     not do; and
//   - a SQL NULL renders as an empty cell, where ScanRows renders the literal
//     text "<nil>". ScanRows scans into *interface{} (scan_row.go:34-36), so a
//     driver NULL leaves an UNTYPED nil whose reflect.Kind is Invalid, not
//     Pointer; the IsNil branch at scan_row.go:69-72 never fires and the
//     default arm formats it as "<nil>" (scan_row.go:97-98). The existing
//     Test_sqlValToString_nilTypedPointer covers a *typed* nil pointer, which
//     is a different value and does render "". Nothing in the repository
//     asserts "<nil>".
type RenderOptions struct {
	// DistinguishNull renders SQL NULL as the literal NULL instead of an
	// empty cell. Set only for InterBase. Note that the zero value already
	// differs from ScanRows here: it renders an empty cell, not "<nil>".
	DistinguishNull bool
	// MaxCellRunes caps rendered cell width. Zero means DefaultMaxCellRunes.
	MaxCellRunes int
}

// DefaultMaxCellRunes is the display cap applied when RenderOptions.MaxCellRunes
// is zero. It is a display limit only, unrelated to the InterBase driver's
// 64 MiB materialisation limit: it exists so a large text BLOB the driver did
// materialise is not pushed whole through JSON-RPC into the editor.
const DefaultMaxCellRunes = 512

const nullCellText = "NULL"

// BlobTypeName is the DatabaseTypeName InterBase reports for a BLOB column.
//
// It is exported because internal/handler gates the 64 MiB materialisation
// hint on the same value (hasBlobColumn, Task 4). Two copies of the literal
// "BLOB" in two packages would be free to drift, and the drift would be
// silent: the hint would simply stop appearing.
const BlobTypeName = "BLOB"

var (
	stringScanType  = reflect.TypeOf("")
	int64ScanType   = reflect.TypeOf(int64(0))
	float64ScanType = reflect.TypeOf(float64(0))
	boolScanType    = reflect.TypeOf(false)
	timeScanType    = reflect.TypeOf(time.Time{})
	bytesScanType   = reflect.TypeOf([]byte(nil))
)

// RenderOptionsFor returns the cell-rendering options for a driver. Only
// InterBase distinguishes NULL from the empty string, because only its driver
// guarantees that empty values stay distinct from NULL through the fetch.
func RenderOptionsFor(driver dialect.DatabaseDriver) RenderOptions {
	if driver == dialect.DatabaseDriverInterBase {
		return RenderOptions{DistinguishNull: true}
	}
	return RenderOptions{}
}

// ScanRowsWithTypes renders rows using the column metadata the driver reports,
// which ScanRows never asks for.
//
// It returns a non-nil *QueryResult together with a non-nil error when the
// fetch fails partway: Rows holds everything scanned before the failure,
// Columns is populated, and Complete is false. It returns (nil, err) only when
// rows.ColumnTypes() itself fails, i.e. when there is nothing to report. The
// caller is expected to render the partial result and then report the error —
// the InterBase driver's 64 MiB BLOB limit is a hard fetch error rather than a
// truncation, so the rows that preceded it are the only clue to which value
// broke the query.
func ScanRowsWithTypes(rows *sql.Rows, opts RenderOptions) (*QueryResult, error) {
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("cannot get query column types, %w", err)
	}

	result := &QueryResult{
		Columns: make([]ColumnMeta, len(columnTypes)),
		Rows:    [][]string{},
	}
	for i, columnType := range columnTypes {
		meta := ColumnMeta{
			Name:             columnType.Name(),
			DatabaseTypeName: strings.ToUpper(columnType.DatabaseTypeName()),
		}
		if strings.TrimSpace(meta.Name) == "" {
			meta.Name = fmt.Sprintf("col%d", i)
		}
		if nullable, ok := columnType.Nullable(); ok {
			meta.Nullable = sql.NullBool{Bool: nullable, Valid: true}
		}
		if length, ok := columnType.Length(); ok {
			meta.Length = sql.NullInt64{Int64: length, Valid: true}
		}
		if precision, scale, ok := columnType.DecimalSize(); ok {
			meta.Precision = sql.NullInt64{Int64: precision, Valid: true}
			meta.Scale = sql.NullInt64{Int64: scale, Valid: true}
		}
		result.Columns[i] = meta
	}

	maxCellRunes := opts.MaxCellRunes
	if maxCellRunes <= 0 {
		maxCellRunes = DefaultMaxCellRunes
	}

	for rows.Next() {
		buffer := make([]interface{}, len(columnTypes))
		for i, columnType := range columnTypes {
			buffer[i] = newScanDest(columnType.ScanType())
		}
		if err := rows.Scan(buffer...); err != nil {
			return result, err
		}

		row := make([]string, len(columnTypes))
		for i, dest := range buffer {
			cell, err := renderCell(dest, result.Columns[i], opts, maxCellRunes)
			if err != nil {
				return result, err
			}
			row[i] = cell
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	result.Complete = true
	return result, nil
}

// newScanDest allocates a NULL-tolerant destination for one column. A plain
// *string or *int64 fails on a NULL value ("converting NULL to string is
// unsupported"), which would destroy exactly the distinction this renderer
// exists to keep, so every concrete type gets a nullable holder. An unknown
// scan type falls back to *interface{}, which is what ScanRows uses for every
// column — so a driver that reports no scan types renders identically to today.
func newScanDest(scanType reflect.Type) interface{} {
	switch scanType {
	case stringScanType:
		return new(sql.NullString)
	case int64ScanType:
		return new(sql.NullInt64)
	case float64ScanType:
		return new(sql.NullFloat64)
	case boolScanType:
		return new(sql.NullBool)
	case timeScanType:
		return new(sql.NullTime)
	case bytesScanType:
		return new([]byte)
	}
	return new(interface{})
}

func renderCell(dest interface{}, meta ColumnMeta, opts RenderOptions, maxCellRunes int) (string, error) {
	switch value := dest.(type) {
	case *sql.NullString:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return capCell(value.String, meta, maxCellRunes), nil
	case *sql.NullInt64:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Int64), nil
	case *sql.NullFloat64:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Float64), nil
	case *sql.NullBool:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Bool), nil
	case *sql.NullTime:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return value.Time.Format(time.RFC3339Nano), nil
	case *[]byte:
		if *value == nil {
			return nullCell(opts), nil
		}
		if meta.DatabaseTypeName == BlobTypeName {
			return fmt.Sprintf("<BLOB %d bytes>", len(*value)), nil
		}
		return capCell(string(*value), meta, maxCellRunes), nil
	case *interface{}:
		if *value == nil {
			return nullCell(opts), nil
		}
		// A driver with no scan-type metadata gets exactly today's rendering.
		text, err := sqlValToString(value)
		if err != nil {
			return "", err
		}
		return capCell(text, meta, maxCellRunes), nil
	}
	return "", fmt.Errorf("unsupported scan destination %T", dest)
}

func nullCell(opts RenderOptions) string {
	if opts.DistinguishNull {
		return nullCellText
	}
	return ""
}

// capCell truncates an over-long rendered cell and says by how much, so the cap
// is never mistaken for missing data.
func capCell(text string, meta ColumnMeta, maxCellRunes int) string {
	total := utf8.RuneCountInString(text)
	if total <= maxCellRunes {
		return text
	}
	return string([]rune(text)[:maxCellRunes]) + fmt.Sprintf("…(truncated, %d characters)", total)
}
