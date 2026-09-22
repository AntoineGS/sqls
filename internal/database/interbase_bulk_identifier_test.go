package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// This file covers the follow-up recorded in
// /tmp/opencode/sqls-bulk/native-diagnosis.md and
// /tmp/opencode/sqls-bulk/driver-followup.md: the native InterBase driver's
// SQL_TEXT decoder caps a CHAR/UNICODE_FSS column's returned length at
// declaredByteWidth/3, corrupting any catalog identifier over ~22 characters.
// The driver itself is unpatched (deleting the cap contradicts
// tests/native_values_test.c:204-243's CHAR padding coverage). The bulk cache
// queries instead project every catalog identifier column through
// CAST(... AS VARCHAR(n)), which the driver already decodes correctly (its
// SQL_VARYING path takes the wire length prefix as authoritative, with no
// charset-width recomputation) — bypassing the defect for this one read path
// only. General-purpose catalog reads (the standalone repository, extended
// catalog accessors) still go through the driver's unfixed CHAR decode.

// --- Static check: every identifier column the four bulk queries project is
// --- CAST to VARCHAR, at the width validated live against NRF01 in
// --- native-diagnosis.md. Non-identifier columns (BLOB sources, integers)
// --- are untouched.

func TestInterBaseBulkQueriesCastEveryIdentifierColumnToVarchar(t *testing.T) {
	cast := func(ref string) string {
		return fmt.Sprintf("CAST(%s AS VARCHAR(%d))", ref, interBaseBulkIdentifierCastWidth)
	}

	cases := []struct {
		query        string
		mustContain  []string
		mustNotMatch []string
	}{
		{
			query:       interBaseBulkRelationsQuery,
			mustContain: []string{cast("r.RDB$RELATION_NAME")},
		},
		{
			query: interBaseBulkColumnsQuery,
			mustContain: []string{
				cast("rf.RDB$RELATION_NAME"),
				cast("rf.RDB$FIELD_NAME"),
				cast("rf.RDB$FIELD_SOURCE"),
				cast("f.RDB$FIELD_NAME"),
				cast("cs.RDB$CHARACTER_SET_NAME"),
				cast("co.RDB$COLLATION_NAME"),
			},
			// BLOB source text is not the identifier bug's shape and must
			// stay a bare projection.
			mustNotMatch: []string{
				"CAST(rf.RDB$DEFAULT_SOURCE",
				"CAST(f.RDB$COMPUTED_SOURCE",
				"CAST(f.RDB$DEFAULT_SOURCE",
			},
		},
		{
			query: interBaseBulkPrimaryKeyFieldsQuery,
			mustContain: []string{
				cast("pk.RDB$RELATION_NAME"),
				cast("s.RDB$FIELD_NAME"),
			},
		},
		{
			query: interBaseBulkForeignKeyFieldsQuery,
			mustContain: []string{
				cast("fk.RDB$CONSTRAINT_NAME"),
				cast("fk.RDB$RELATION_NAME"),
				cast("fs.RDB$FIELD_NAME"),
				cast("pk.RDB$RELATION_NAME"),
				cast("ps.RDB$FIELD_NAME"),
			},
		},
	}

	for i, tc := range cases {
		for _, want := range tc.mustContain {
			if !strings.Contains(tc.query, want) {
				t.Errorf("query %d: missing %q in:\n%s", i, want, tc.query)
			}
		}
		for _, unwanted := range tc.mustNotMatch {
			if strings.Contains(tc.query, unwanted) {
				t.Errorf("query %d: unexpectedly cast a non-identifier column with %q in:\n%s", i, unwanted, tc.query)
			}
		}
	}
}

// TestInterBaseBulkQueriesKeepJoinsAndOrderOnRawColumns pins the requirement
// that only the SELECT list is rewritten: JOIN and ORDER BY must still
// compare/sort the original catalog CHAR columns, not the CAST projection, so
// blank-padded CHAR comparison semantics (relied on elsewhere, e.g. review.md
// residual concern 3) are unchanged.
func TestInterBaseBulkQueriesKeepJoinsAndOrderOnRawColumns(t *testing.T) {
	mustContainRaw := []struct{ query, want string }{
		{interBaseBulkRelationsQuery, "ORDER BY r.RDB$RELATION_NAME"},
		{interBaseBulkColumnsQuery, "JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = rf.RDB$RELATION_NAME"},
		{interBaseBulkColumnsQuery, "LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE"},
		{interBaseBulkColumnsQuery, "ORDER BY rf.RDB$RELATION_NAME, rf.RDB$FIELD_POSITION"},
		{interBaseBulkPrimaryKeyFieldsQuery, "JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = pk.RDB$RELATION_NAME"},
		{interBaseBulkForeignKeyFieldsQuery, "JOIN RDB$REF_CONSTRAINTS rc ON rc.RDB$CONSTRAINT_NAME = fk.RDB$CONSTRAINT_NAME"},
		{interBaseBulkForeignKeyFieldsQuery, "ORDER BY fk.RDB$CONSTRAINT_NAME, fs.RDB$FIELD_POSITION"},
	}
	for _, tc := range mustContainRaw {
		if !strings.Contains(tc.query, tc.want) {
			t.Errorf("missing raw-column clause %q in:\n%s", tc.want, tc.query)
		}
	}
}

// --- Regression: the enforcing/referenced index's own RDB$SYSTEM_FLAG must
// --- be checked, matching schema.Catalog.Index() (catalog_extended.go), so
// --- the bulk PK/FK reads cannot surface an index a legacy/hand-flagged
// --- catalog marks system on an otherwise ordinary user relation. Review.md
// --- residual concern 1.

func TestInterBaseBulkQueriesFilterSystemFlaggedIndexes(t *testing.T) {
	mustContain := []struct{ query, want string }{
		{interBaseBulkPrimaryKeyFieldsQuery, "COALESCE(pi.RDB$SYSTEM_FLAG, 0) = 0"},
		{interBaseBulkForeignKeyFieldsQuery, "COALESCE(fi.RDB$SYSTEM_FLAG, 0) = 0"},
		{interBaseBulkForeignKeyFieldsQuery, "COALESCE(pi.RDB$SYSTEM_FLAG, 0) = 0"},
	}
	for _, tc := range mustContain {
		if !strings.Contains(tc.query, tc.want) {
			t.Errorf("missing index system-flag filter %q in:\n%s", tc.want, tc.query)
		}
	}
}

func TestInterBaseBulkPrimaryKeysExcludeASystemFlaggedEnforcingIndex(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	if _, err := db.Exec(`UPDATE "RDB$INDICES" SET "RDB$SYSTEM_FLAG" = 1 WHERE "RDB$INDEX_NAME" = 'IDX_PK_T001'`); err != nil {
		t.Fatalf("flag the enforcing index as system: %v", err)
	}
	ctx := context.Background()

	primaryKeys, err := interBaseBulkPrimaryKeys(ctx, db)
	if err != nil {
		t.Fatalf("interBaseBulkPrimaryKeys() error = %v", err)
	}
	if _, ok := primaryKeys[interBaseRelationColumnKey("T001", "KEY_A")]; ok {
		t.Errorf("T001.KEY_A reported as a primary-key field although its enforcing index IDX_PK_T001 is RDB$SYSTEM_FLAG=1")
	}
	if _, ok := primaryKeys[interBaseRelationColumnKey("T002", "KEY_A")]; !ok {
		t.Errorf("T002.KEY_A missing from primary keys; only T001's index was flagged system")
	}
}

func TestInterBaseBulkForeignKeysExcludeASystemFlaggedEnforcingIndex(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	if _, err := db.Exec(`UPDATE "RDB$INDICES" SET "RDB$SYSTEM_FLAG" = 1 WHERE "RDB$INDEX_NAME" = 'IDX_FK_T002'`); err != nil {
		t.Fatalf("flag the FK enforcing index as system: %v", err)
	}
	ctx := context.Background()

	foreignKeys, err := interBaseBulkForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("interBaseBulkForeignKeys() error = %v", err)
	}
	for _, fk := range foreignKeys {
		if fk.relationName == "T002" {
			t.Errorf("T002's foreign key reported although its enforcing index IDX_FK_T002 is RDB$SYSTEM_FLAG=1: %#v", fk)
		}
	}
}

func TestInterBaseBulkForeignKeysExcludeASystemFlaggedReferencedIndex(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	if _, err := db.Exec(`UPDATE "RDB$INDICES" SET "RDB$SYSTEM_FLAG" = 1 WHERE "RDB$INDEX_NAME" = 'IDX_PK_T001'`); err != nil {
		t.Fatalf("flag the referenced index as system: %v", err)
	}
	ctx := context.Background()

	foreignKeys, err := interBaseBulkForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("interBaseBulkForeignKeys() error = %v", err)
	}
	for _, fk := range foreignKeys {
		if fk.relationName == "T002" {
			t.Errorf("T002's foreign key reported although its referenced index IDX_PK_T001 is RDB$SYSTEM_FLAG=1: %#v", fk)
		}
	}
}

// --- Long, prefix-colliding identifiers: proves the bulk loaders group by
// --- full names, not by any truncated prefix, both through the SQLite
// --- fixture (application-level grouping) and through a driver seam that
// --- reproduces the confirmed native truncation for a bare CHAR reference
// --- (driver-level bypass).

func TestInterBaseBulkForeignKeysDoNotMergeLongCollidingConstraintNames(t *testing.T) {
	// Both names share the same first 22 characters — the exact byte count
	// the native driver's flawed decoder would leave a bare CHAR(67)
	// UNICODE_FSS column with (native-diagnosis.md). The SQLite fixture
	// itself never truncates, so this pins the Go-side grouping key (full
	// constraint identity) against the same collision shape confirmed live.
	longA := "ELEMCOUNTRYLABELS_REF_COUNTRYID"
	longB := "ELEMCOUNTRYLABELS_REF_ELEMENTID"
	if longA[:22] != longB[:22] {
		t.Fatalf("fixture setup error: the two constraint names must share a 22-character prefix, got %q and %q", longA[:22], longB[:22])
	}

	db := openInterBaseScalableFixture(t, 3)
	rename := func(oldName, newName string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE "RDB$RELATION_CONSTRAINTS" SET "RDB$CONSTRAINT_NAME" = ? WHERE "RDB$CONSTRAINT_NAME" = ?`, newName, oldName); err != nil {
			t.Fatalf("rename constraint %s -> %s: %v", oldName, newName, err)
		}
		if _, err := db.Exec(`UPDATE "RDB$REF_CONSTRAINTS" SET "RDB$CONSTRAINT_NAME" = ? WHERE "RDB$CONSTRAINT_NAME" = ?`, newName, oldName); err != nil {
			t.Fatalf("rename ref constraint %s -> %s: %v", oldName, newName, err)
		}
	}
	rename("FK_T002", longA)
	// A second relation with its own FK gets the colliding-prefix name so a
	// truncation-prone grouping key would merge the two constraints' fields.
	rename("FK_T003", longB)

	ctx := context.Background()
	foreignKeys, err := interBaseBulkForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("interBaseBulkForeignKeys() error = %v", err)
	}
	if len(foreignKeys) != 2 {
		t.Fatalf("got %d foreign keys, want 2 (one per colliding-prefix constraint name), got %#v", len(foreignKeys), foreignKeys)
	}
	byRelation := map[string]interBaseForeignKeyMapping{}
	for _, fk := range foreignKeys {
		byRelation[fk.relationName] = fk
	}
	t002, ok := byRelation["T002"]
	if !ok || !reflect.DeepEqual(t002.columns, []string{"REF_A", "REF_B"}) {
		t.Errorf("T002 foreign key columns = %#v, want [REF_A REF_B] (not merged with T003's)", byRelation["T002"])
	}
	t003, ok := byRelation["T003"]
	if !ok || !reflect.DeepEqual(t003.columns, []string{"REF_A", "REF_B"}) {
		t.Errorf("T003 foreign key columns = %#v, want [REF_A REF_B] (not merged with T002's)", byRelation["T003"])
	}
}

// interBaseCharTruncationColumnValues supplies the full, untruncated stored
// value for each "alias.RDB$COLUMN" reference a query selects — standing in
// for one real InterBase row.
type interBaseCharTruncationColumnValues map[string]string

// interBaseCharTruncationDeclaredWidth mirrors the live-confirmed catalog
// domain width from native-diagnosis.md (RDB$FIELDS: RDB$FIELD_LENGTH=67,
// RDB$CHARACTER_SET_ID=3/UNICODE_FSS) that every bare identifier column in
// this schema declares.
const interBaseCharTruncationDeclaredWidth = 67

// interBaseSimulateNativeCharDecode reproduces the confirmed defect
// (native.c:9200-9204 per native-diagnosis.md): source_length is capped at
// declaredWidth/charsetMaxBytesPerChar (67/3=22 for UNICODE_FSS) before
// conversion, regardless of the value's real length.
func interBaseSimulateNativeCharDecode(value string) string {
	limit := interBaseCharTruncationDeclaredWidth / 3
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// interBaseSelectColumn is one parsed SELECT-list expression: either a bare
// "alias.RDB$COLUMN" reference (subject to the simulated defect) or a
// CAST(alias.RDB$COLUMN AS VARCHAR(n)) expression (bounded only by n, exactly
// how the driver's already-correct SQL_VARYING path behaves).
type interBaseSelectColumn struct {
	raw    string
	width  int
	casted bool
}

var interBaseCastPattern = regexp.MustCompile(`^CAST\((\S+)\s+AS\s+VARCHAR\((\d+)\)\)$`)

func interBaseParseSelectColumn(expr string) interBaseSelectColumn {
	expr = strings.Join(strings.Fields(expr), " ")
	if m := interBaseCastPattern.FindStringSubmatch(expr); m != nil {
		width, _ := strconv.Atoi(m[2])
		return interBaseSelectColumn{raw: m[1], width: width, casted: true}
	}
	return interBaseSelectColumn{raw: expr}
}

// interBaseParseSelectList extracts the top-level comma-separated SELECT-list
// expressions from one of the bulk query constants. It is a test-only seam,
// not a general SQL parser: it assumes the query's SELECT list is followed by
// a line starting with "FROM ", true of every constant in
// interbase_cache_bulk.go.
func interBaseParseSelectList(query string) ([]interBaseSelectColumn, error) {
	const selectPrefix = "SELECT"
	start := strings.Index(query, selectPrefix)
	if start < 0 {
		return nil, fmt.Errorf("query has no SELECT: %s", query)
	}
	rest := query[start+len(selectPrefix):]
	end := strings.Index(rest, "\nFROM ")
	if end < 0 {
		return nil, fmt.Errorf("query has no FROM after SELECT: %s", query)
	}
	list := rest[:end]

	var columns []interBaseSelectColumn
	depth := 0
	last := 0
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				columns = append(columns, interBaseParseSelectColumn(list[last:i]))
				last = i + 1
			}
		}
	}
	columns = append(columns, interBaseParseSelectColumn(list[last:]))
	return columns, nil
}

// interBaseCharTruncationConn is a database/sql/driver seam reproducing the
// confirmed native decoder defect for a bare CHAR column reference, while
// passing a CAST(... AS VARCHAR(n)) expression through untouched. It does not
// implement joins or WHERE filtering: canned row values are supplied
// directly, keyed by the exact column reference the real query text selects,
// which is enough to prove the property against the actual SQL sqls emits
// without reimplementing a relational engine.
type interBaseCharTruncationConn struct {
	rows []interBaseCharTruncationColumnValues
}

func (interBaseCharTruncationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("interBaseCharTruncationConn: Prepare not implemented")
}
func (interBaseCharTruncationConn) Close() error { return nil }
func (interBaseCharTruncationConn) Begin() (driver.Tx, error) {
	return nil, errors.New("interBaseCharTruncationConn: Begin not implemented")
}

func (c interBaseCharTruncationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	columns, err := interBaseParseSelectList(query)
	if err != nil {
		return nil, err
	}
	return &interBaseCharTruncationRows{columns: columns, rows: c.rows}, nil
}

type interBaseCharTruncationRows struct {
	columns []interBaseSelectColumn
	rows    []interBaseCharTruncationColumnValues
	index   int
}

func (r *interBaseCharTruncationRows) Columns() []string {
	names := make([]string, len(r.columns))
	for i, c := range r.columns {
		names[i] = c.raw
	}
	return names
}

func (r *interBaseCharTruncationRows) Close() error { return nil }

func (r *interBaseCharTruncationRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.index]
	r.index++
	for i, col := range r.columns {
		value, ok := row[col.raw]
		if !ok {
			dest[i] = nil
			continue
		}
		if col.casted {
			if len(value) > col.width {
				value = value[:col.width]
			}
			dest[i] = value
			continue
		}
		dest[i] = interBaseSimulateNativeCharDecode(value)
	}
	return nil
}

var (
	registerInterBaseCharTruncationDriverOnce sync.Once
)

func openInterBaseCharTruncationDB(t *testing.T, rows []interBaseCharTruncationColumnValues) *sql.DB {
	t.Helper()
	registerInterBaseCharTruncationDriverOnce.Do(func() {
		sql.Register("interbase_char_truncation_test", interBaseCharTruncationDriver{})
	})
	db, err := sql.Open("interbase_char_truncation_test", "")
	if err != nil {
		t.Fatalf("sql.Open(interbase_char_truncation_test) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	interBaseCharTruncationDriverRows = rows
	return db
}

// interBaseCharTruncationDriverRows is read once per Open call. A package
// driver registered with sql.Register cannot take per-test constructor
// arguments, so the fixture rows travel through this package variable; tests
// using it must not run in parallel with each other (matching the existing
// interBaseFixturePrepares convention in interbase_catalog_test.go).
var interBaseCharTruncationDriverRows []interBaseCharTruncationColumnValues

type interBaseCharTruncationDriver struct{}

func (interBaseCharTruncationDriver) Open(string) (driver.Conn, error) {
	return interBaseCharTruncationConn{rows: interBaseCharTruncationDriverRows}, nil
}

// TestInterBaseBulkRelationsQueryBypassesTheNativeCharTruncationDefect runs
// the actual interBaseBulkRelationsQuery constant through the truncating
// driver seam and shows its CAST wrapping returns the full 29-character
// value the live NRF01 probe recorded, while the same SELECT list without the
// CAST — reproduced here only to demonstrate the seam is real, not a no-op —
// reproduces the exact 22-character truncation native-diagnosis.md recorded.
func TestInterBaseBulkRelationsQueryBypassesTheNativeCharTruncationDefect(t *testing.T) {
	const full = "IMPORT_SHOPIFY_OPTION_VALUES" // 29 chars, the live-confirmed case
	const truncated = "IMPORT_SHOPIFY_OPTION_"  // 22 chars, native-diagnosis.md's recorded defect output

	db := openInterBaseCharTruncationDB(t, []interBaseCharTruncationColumnValues{
		{"r.RDB$RELATION_NAME": full},
	})
	ctx := context.Background()

	var got string
	if err := db.QueryRowContext(ctx, interBaseBulkRelationsQuery).Scan(&got); err != nil {
		t.Fatalf("QueryRowContext(interBaseBulkRelationsQuery) error = %v", err)
	}
	if got != full {
		t.Errorf("interBaseBulkRelationsQuery (CAST to VARCHAR) = %q, want the full value %q; the native CHAR-decode cap is not bypassed", got, full)
	}

	rawQuery := strings.Replace(interBaseBulkRelationsQuery,
		fmt.Sprintf("CAST(r.RDB$RELATION_NAME AS VARCHAR(%d))", interBaseBulkIdentifierCastWidth),
		"r.RDB$RELATION_NAME", 1)
	if rawQuery == interBaseBulkRelationsQuery {
		t.Fatal("test setup error: could not strip the CAST wrapping from interBaseBulkRelationsQuery")
	}
	var gotRaw string
	if err := db.QueryRowContext(ctx, rawQuery).Scan(&gotRaw); err != nil {
		t.Fatalf("QueryRowContext(raw form) error = %v", err)
	}
	if gotRaw != truncated {
		t.Errorf("bare CHAR reference through the truncation seam = %q, want the simulated defect's %q; the seam does not reproduce the native bug", gotRaw, truncated)
	}
}

// TestInterBaseBulkForeignKeyFieldsQueryBypassesTheNativeCharTruncationDefect
// runs the actual interBaseBulkForeignKeyFieldsQuery constant, whose
// constraint-name projection is the exact live FK-grouping collision
// native-diagnosis.md confirmed, through the same truncating seam and asserts
// the two colliding-prefix constraint names decode fully distinct.
func TestInterBaseBulkForeignKeyFieldsQueryBypassesTheNativeCharTruncationDefect(t *testing.T) {
	nameA := "ELEMCOUNTRYLABELS_REF_COUNTRYID" // the live-confirmed collision pair
	nameB := "ELEMCOUNTRYLABELS_REF_ELEMENTID"
	if nameA[:22] != nameB[:22] {
		t.Fatalf("fixture setup error: expected a 22-character shared prefix")
	}

	db := openInterBaseCharTruncationDB(t, []interBaseCharTruncationColumnValues{
		{
			"fk.RDB$CONSTRAINT_NAME": nameA,
			"fk.RDB$RELATION_NAME":   "COUNTRYLABELS",
			"fs.RDB$FIELD_NAME":      "COUNTRY_ID",
			"pk.RDB$RELATION_NAME":   "COUNTRIES",
			"ps.RDB$FIELD_NAME":      "ID",
		},
		{
			"fk.RDB$CONSTRAINT_NAME": nameB,
			"fk.RDB$RELATION_NAME":   "COUNTRYLABELS",
			"fs.RDB$FIELD_NAME":      "ELEMENT_ID",
			"pk.RDB$RELATION_NAME":   "ELEMENTS",
			"ps.RDB$FIELD_NAME":      "ID",
		},
	})
	ctx := context.Background()

	foreignKeys, err := interBaseBulkForeignKeys(ctx, db)
	if err != nil {
		t.Fatalf("interBaseBulkForeignKeys() error = %v", err)
	}
	if len(foreignKeys) != 2 {
		t.Fatalf("got %d foreign keys through the truncating seam, want 2 distinct constraints (%q and %q must not collide); got %#v",
			len(foreignKeys), nameA, nameB, foreignKeys)
	}
	fields := map[string]bool{}
	for _, fk := range foreignKeys {
		if len(fk.columns) != 1 {
			t.Errorf("foreign key columns = %#v, want exactly one field per un-merged constraint", fk.columns)
			continue
		}
		fields[fk.columns[0]] = true
	}
	if !fields["COUNTRY_ID"] || !fields["ELEMENT_ID"] {
		t.Errorf("foreign keys = %#v, want one for COUNTRY_ID and one for ELEMENT_ID, not merged", foreignKeys)
	}
}
