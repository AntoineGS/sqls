package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	"interbase-go/schema"
)

// The schema package's domain list filters system domains with
// "RDB$FIELD_NAME NOT STARTING WITH 'RDB$'" (schema/catalog_extended.go:304).
// STARTING WITH is InterBase syntax and SQLite rejects it outright, so the
// fixture is reached through a driver that rewrites that one clause into the
// LIKE form SQLite understands. Nothing in production does this: it exists so
// the pure-Go catalog reader can be exercised without a server.
type interBaseFixtureDriver struct{ inner driver.Driver }

func (d interBaseFixtureDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return interBaseFixtureConn{Conn: conn}, nil
}

type interBaseFixtureConn struct{ driver.Conn }

func interBaseFixtureRewrite(query string) string {
	return strings.ReplaceAll(query, "NOT STARTING WITH 'RDB$'", "NOT LIKE 'RDB$%'")
}

// interBaseFixturePrepares counts every statement the fixture prepares. Because
// this wrapper forces all traffic through PrepareContext (see below), the count
// is the exact round-trip count for a catalog build, which is what turns Task
// 8's N+1 claim into a measurement instead of an argument.
var interBaseFixturePrepares atomic.Int64

// interBaseFixtureCountPrepares resets the counter and returns a reader for it.
// Tests using it must not run in parallel with each other.
func interBaseFixtureCountPrepares(t *testing.T) func() int64 {
	t.Helper()
	interBaseFixturePrepares.Store(0)
	return interBaseFixturePrepares.Load
}

// Embedding driver.Conn promotes only the driver.Conn methods, so this wrapper
// deliberately does not satisfy driver.QueryerContext; database/sql therefore
// routes every statement through PrepareContext, where the rewrite applies.
func (c interBaseFixtureConn) Prepare(query string) (driver.Stmt, error) {
	interBaseFixturePrepares.Add(1)
	return c.Conn.Prepare(interBaseFixtureRewrite(query))
}

func (c interBaseFixtureConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	interBaseFixturePrepares.Add(1)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, interBaseFixtureRewrite(query))
	}
	return c.Conn.Prepare(interBaseFixtureRewrite(query))
}

func (c interBaseFixtureConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func init() {
	sql.Register("sqlite3_interbase_catalog", interBaseFixtureDriver{inner: &sqlite3.SQLiteDriver{}})
}

var interBaseFixtureSequence atomic.Int64

// openInterBaseSchemaFixture builds an in-memory RDB$ catalog that the schema
// package reads exactly as it reads a real one.
//
// Identifiers used as a join key or a bind parameter are stored UNPADDED.
// InterBase CHAR comparison pads both operands, so 'CUSTOMER' = 'CUSTOMER   '
// there; SQLite compares TEXT byte for byte, and schema re-queries columns,
// index segments, procedure parameters and function arguments with the name it
// already trimmed. Padded keys therefore yield relations with zero columns.
// Display-only columns stay padded so the trimming behavior remains pinned.
//
// The database is shared-cache in-memory with a unique name per call, so a
// read-only transaction and a second pooled connection can be open at once.
func openInterBaseSchemaFixture(t *testing.T) *sql.DB {
	t.Helper()

	name := fmt.Sprintf("file:interbase_catalog_%d?mode=memory&cache=shared", interBaseFixtureSequence.Add(1))
	db, err := sql.Open("sqlite3_interbase_catalog", name)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3_interbase_catalog) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxIdleConns(4)

	for _, statement := range interBaseFixtureTables {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create catalog fixture table: %v: %s", err, statement)
		}
	}

	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("insert catalog fixture row: %v: %s", err, statement)
		}
	}

	// RDB$RELATIONS: three tables, one view, one system relation.
	relation := func(name string, id int, viewBLR, viewSource any, systemFlag int) {
		insert(`INSERT INTO "RDB$RELATIONS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, id, viewSource, nil, nil, interBaseFixed("SYSDBA"), nil, 8, 1, nil, 0,
			interBaseFixed("PERSISTENT"), systemFlag, viewBLR)
	}
	relation("CHILD", 1, nil, nil, 0)
	relation("CUSTOMER", 2, nil, nil, 0)
	relation("CUSTOMER_VIEW", 3, "view blr", "SELECT ID FROM CUSTOMER", 0)
	relation("PARENT", 4, nil, nil, 0)
	relation("RDB$SYSTEM", 5, nil, nil, 1)

	// RDB$RELATION_FIELDS. ORPHAN's field source has no RDB$FIELDS row: the
	// old inner JOIN dropped it, schema LEFT JOINs and keeps it.
	field := func(relationName, name, source string, position int, nullFlag, defaultSource, computedFlag any) {
		_ = computedFlag
		insert(`INSERT INTO "RDB$RELATION_FIELDS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			interBaseFixed(name), relationName, source, position, nil, position, nil, 0, nil,
			nullFlag, defaultSource, nil, nil, nil)
	}
	field("CHILD", "CHILD_B", "CHILD_B", 0, nil, nil, nil)
	field("CHILD", "CHILD_A", "CHILD_A", 1, nil, nil, nil)
	field("CHILD", "ORPHAN", "NO_SUCH_DOMAIN", 2, nil, nil, nil)
	field("CUSTOMER", "ID", "CUSTOMER_ID", 0, 1, nil, nil)
	field("CUSTOMER", "CODE", "CUSTOMER_CODE", 1, nil, nil, nil)
	field("CUSTOMER", "CREATED", "CUSTOMER_CREATED", 2, nil, nil, nil)
	field("CUSTOMER", "AMOUNT", "CUSTOMER_AMOUNT", 3, nil, nil, nil)
	field("CUSTOMER", "LABEL", "CUSTOMER_LABEL", 4, nil, " DEFAULT '  seeded  '   ", nil)
	field("CUSTOMER", "INHERITED", "CUSTOMER_INHERITED", 5, nil, nil, nil)
	field("CUSTOMER", "OVERRIDE", "CUSTOMER_OVERRIDE", 6, nil, " DEFAULT 'column' ", nil)
	field("CUSTOMER", "DEFAULT_NULL", "CUSTOMER_DEFAULT_NULL", 7, nil, " DEFAULT NULL ", nil)
	field("CUSTOMER", "REQUIRED", "CUSTOMER_REQUIRED", 8, 1, nil, nil)
	field("CUSTOMER", "DOUBLE_AMOUNT", "CUSTOMER_DOUBLE_AMOUNT", 9, nil, nil, nil)
	field("CUSTOMER", "TOTAL", "CUSTOMER_TOTAL", 10, nil, nil, nil)
	field("CUSTOMER_VIEW", "VIEW_ID", "CUSTOMER_ID", 0, nil, nil, nil)
	field("PARENT", "PARENT_B", "PARENT_B", 0, nil, nil, nil)
	field("PARENT", "PARENT_A", "PARENT_A", 1, nil, nil, nil)

	// RDB$FIELDS. Column order matches schema's domain projection exactly.
	domain := func(name string, fieldType int, subType, length, scale, precision, characterLength,
		characterSetID, collationID, nullFlag, defaultSource, validationSource, computedSource, dimensions any) {
		insert(`INSERT INTO "RDB$FIELDS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, validationSource, computedSource, defaultSource, length, scale, fieldType, subType,
			nil, 0, nil, nil, nil, nil, dimensions, nullFlag, characterLength, collationID,
			characterSetID, precision)
	}
	domain("CHILD_B", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CHILD_A", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_ID", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_CODE", 14, 0, 10, 0, nil, 10, 4, 2, nil, nil, nil, nil, nil)
	domain("CUSTOMER_CREATED", 35, 0, 8, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_AMOUNT", 8, 1, 4, -2, 9, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_LABEL", 37, 0, 20, 0, nil, 20, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_INHERITED", 37, 0, 20, 0, nil, 20, nil, nil, 1, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_OVERRIDE", 37, 0, 20, 0, nil, 20, nil, nil, nil, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_DEFAULT_NULL", 37, 0, 20, 0, nil, 20, nil, nil, nil, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_REQUIRED", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_DOUBLE_AMOUNT", 27, 1, 8, 0, 15, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_TOTAL", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, "COMPUTED BY (AMOUNT * 2)", nil)
	domain("PARENT_B", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("PARENT_A", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	// A user domain and a system domain, for the procedure-parameter test.
	domain("EMAIL_ADDRESS", 37, 0, 100, 0, nil, 100, nil, nil, 1, " DEFAULT 'a@b' ",
		"CHECK (VALUE LIKE '%@%')", nil, nil)
	domain("RDB$1", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("LEGACY_FLAG", 7, 0, 2, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	insert(`INSERT INTO "RDB$CHARACTER_SETS" VALUES (?,?)`, 4, interBaseFixed("UTF8"))
	insert(`INSERT INTO "RDB$COLLATIONS" VALUES (?,?,?)`, 4, 2, interBaseFixed("UNICODE"))
	insert(`INSERT INTO "RDB$VIEW_RELATIONS" VALUES (?,?,?)`, "CUSTOMER_VIEW", 1, "CUSTOMER")

	constraint := func(name, kind, relationName, indexName string) {
		insert(`INSERT INTO "RDB$RELATION_CONSTRAINTS" VALUES (?,?,?,?,?,?)`,
			name, kind, relationName, nil, nil, indexName)
	}
	constraint("FK_CHILD", "FOREIGN KEY", "CHILD", "IDX_CHILD_FK")
	constraint("PK_CUSTOMER", "PRIMARY KEY", "CUSTOMER", "IDX_CUSTOMER_PK")
	constraint("PK_PARENT", "PRIMARY KEY", "PARENT", "IDX_PARENT_PK")
	insert(`INSERT INTO "RDB$REF_CONSTRAINTS" VALUES (?,?,?,?,?)`,
		"FK_CHILD", "PK_PARENT", nil, "RESTRICT", "RESTRICT")

	// RDB$INDEX_TYPE must be 0 (ascending), not NULL: Index.GenerateDDL
	// refuses a NULL direction, and Task 7 generates DDL for an index.
	index := func(name, relationName string, id int, uniqueFlag, inactive, segmentCount any) {
		insert(`INSERT INTO "RDB$INDICES" VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, relationName, id, uniqueFlag, nil, segmentCount, inactive, 0, nil, 0, nil, nil)
	}
	index("IDX_CHILD_FK", "CHILD", 1, 0, 0, 2)
	index("IDX_CUSTOMER_CODE", "CUSTOMER", 2, 1, 1, 1)
	index("IDX_CUSTOMER_PK", "CUSTOMER", 3, 1, 0, 1)
	index("IDX_PARENT_PK", "PARENT", 4, 1, 0, 2)

	segment := func(indexName, fieldName string, position int) {
		insert(`INSERT INTO "RDB$INDEX_SEGMENTS" VALUES (?,?,?,?)`,
			indexName, interBaseFixed(fieldName), position, nil)
	}
	segment("IDX_CUSTOMER_PK", "ID", 0)
	segment("IDX_PARENT_PK", "PARENT_B", 0)
	segment("IDX_PARENT_PK", "PARENT_A", 1)
	segment("IDX_CHILD_FK", "CHILD_B", 0)
	segment("IDX_CHILD_FK", "CHILD_A", 1)
	segment("IDX_CUSTOMER_CODE", "CODE", 0)

	insert(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		"ADD_CUSTOMER", 1, 2, 1, nil, "BEGIN NEW_ID = 1; END", nil, interBaseFixed("SYSDBA"), 0)
	parameter := func(name string, number, parameterType int, fieldSource string) {
		insert(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" VALUES (?,?,?,?,?,?,?)`,
			interBaseFixed(name), "ADD_CUSTOMER", number, parameterType, fieldSource, nil, 0)
	}
	parameter("EMAIL", 0, 0, "EMAIL_ADDRESS")
	parameter("CODE", 1, 0, "RDB$1")
	parameter("NEW_ID", 0, 1, "CUSTOMER_ID")

	// Trigger type 1 decodes to "BEFORE INSERT", 17 to "BEFORE INSERT OR
	// UPDATE", and 4096 sets a bit the decoder rejects, which is the
	// undecodable case. A NULL relation name is a database-level trigger.
	trigger := func(name string, relationName any, triggerType any, inactive int) {
		insert(`INSERT INTO "RDB$TRIGGERS" VALUES (?,?,?,?,?,?,?,?,?)`,
			name, relationName, 0, triggerType, "AS BEGIN END", nil, inactive, 0, 0)
	}
	trigger("CUSTOMER_BI", "CUSTOMER", 1, 0)
	trigger("CUSTOMER_MULTI", "CUSTOMER", 17, 1)
	trigger("CUSTOMER_ODD", "CUSTOMER", 4096, 0)
	trigger("DB_CONNECT", nil, nil, 0)

	insert(`INSERT INTO "RDB$GENERATORS" VALUES (?,?,?)`, "GEN_CUSTOMER_ID", 1, 0)

	// RDB$RETURN_ARGUMENT is 1: the return value IS input argument 1, which
	// must still appear exactly once in Arguments. Every argument has a NULL
	// RDB$CHARACTER_LENGTH, which is what every measured production row has.
	insert(`INSERT INTO "RDB$FUNCTIONS" VALUES (?,?,?,?,?,?,?)`,
		"F_LTRIM", 0, nil, interBaseFixed("ib_udf"), interBaseFixed("IB_LTRIM"), 1, 0)
	argument := func(position, mechanism, fieldLength, fieldType int, characterSetID any) {
		insert(`INSERT INTO "RDB$FUNCTION_ARGUMENTS" VALUES (?,?,?,?,?,?,?,?,?,?)`,
			"F_LTRIM", position, mechanism, fieldLength, 0, fieldType, 0, characterSetID, nil, nil)
	}
	argument(1, 1, 255, 40, 0) // CSTRING(255): renders from RDB$FIELD_LENGTH
	argument(2, 1, 10, 14, 0)  // CHAR: no character length, renders ""
	argument(3, 1, 4, 8, nil)  // INTEGER

	return db
}

var interBaseFixtureTables = []string{
	`CREATE TABLE "RDB$RELATIONS" ("RDB$RELATION_NAME" TEXT, "RDB$RELATION_ID" INTEGER, "RDB$VIEW_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$SECURITY_CLASS" TEXT, "RDB$OWNER_NAME" TEXT, "RDB$DEFAULT_CLASS" TEXT, "RDB$DBKEY_LENGTH" INTEGER, "RDB$FORMAT" INTEGER, "RDB$EXTERNAL_FILE" TEXT, "RDB$FLAGS" INTEGER, "RDB$RELATION_TYPE" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$VIEW_BLR" TEXT)`,
	`CREATE TABLE "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$FIELD_SOURCE" TEXT, "RDB$FIELD_POSITION" INTEGER, "RDB$UPDATE_FLAG" INTEGER, "RDB$FIELD_ID" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$SECURITY_CLASS" TEXT, "RDB$NULL_FLAG" INTEGER, "RDB$DEFAULT_SOURCE" TEXT, "RDB$COLLATION_ID" INTEGER, "RDB$BASE_FIELD" TEXT, "RDB$VIEW_CONTEXT" INTEGER)`,
	`CREATE TABLE "RDB$FIELDS" ("RDB$FIELD_NAME" TEXT, "RDB$VALIDATION_SOURCE" TEXT, "RDB$COMPUTED_SOURCE" TEXT, "RDB$DEFAULT_SOURCE" TEXT, "RDB$FIELD_LENGTH" INTEGER, "RDB$FIELD_SCALE" INTEGER, "RDB$FIELD_TYPE" INTEGER, "RDB$FIELD_SUB_TYPE" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$SEGMENT_LENGTH" INTEGER, "RDB$EXTERNAL_LENGTH" INTEGER, "RDB$EXTERNAL_SCALE" INTEGER, "RDB$EXTERNAL_TYPE" INTEGER, "RDB$DIMENSIONS" INTEGER, "RDB$NULL_FLAG" INTEGER, "RDB$CHARACTER_LENGTH" INTEGER, "RDB$COLLATION_ID" INTEGER, "RDB$CHARACTER_SET_ID" INTEGER, "RDB$FIELD_PRECISION" INTEGER)`,
	`CREATE TABLE "RDB$CHARACTER_SETS" ("RDB$CHARACTER_SET_ID" INTEGER, "RDB$CHARACTER_SET_NAME" TEXT)`,
	`CREATE TABLE "RDB$COLLATIONS" ("RDB$CHARACTER_SET_ID" INTEGER, "RDB$COLLATION_ID" INTEGER, "RDB$COLLATION_NAME" TEXT)`,
	`CREATE TABLE "RDB$VIEW_RELATIONS" ("RDB$VIEW_NAME" TEXT, "RDB$VIEW_CONTEXT" INTEGER, "RDB$RELATION_NAME" TEXT)`,
	`CREATE TABLE "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$CONSTRAINT_TYPE" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$DEFERRABLE" TEXT, "RDB$INITIALLY_DEFERRED" TEXT, "RDB$INDEX_NAME" TEXT)`,
	`CREATE TABLE "RDB$REF_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$CONST_NAME_UQ" TEXT, "RDB$MATCH_OPTION" TEXT, "RDB$UPDATE_RULE" TEXT, "RDB$DELETE_RULE" TEXT)`,
	`CREATE TABLE "RDB$CHECK_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$TRIGGER_NAME" TEXT)`,
	`CREATE TABLE "RDB$INDICES" ("RDB$INDEX_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$INDEX_ID" INTEGER, "RDB$UNIQUE_FLAG" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SEGMENT_COUNT" INTEGER, "RDB$INDEX_INACTIVE" INTEGER, "RDB$INDEX_TYPE" INTEGER, "RDB$FOREIGN_KEY" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$EXPRESSION_SOURCE" TEXT, "RDB$STATISTICS" REAL)`,
	`CREATE TABLE "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME" TEXT, "RDB$FIELD_NAME" TEXT, "RDB$FIELD_POSITION" INTEGER, "RDB$STATISTICS" REAL)`,
	`CREATE TABLE "RDB$PROCEDURES" ("RDB$PROCEDURE_NAME" TEXT, "RDB$PROCEDURE_ID" INTEGER, "RDB$PROCEDURE_INPUTS" INTEGER, "RDB$PROCEDURE_OUTPUTS" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$PROCEDURE_SOURCE" TEXT, "RDB$SECURITY_CLASS" TEXT, "RDB$OWNER_NAME" TEXT, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$PROCEDURE_PARAMETERS" ("RDB$PARAMETER_NAME" TEXT, "RDB$PROCEDURE_NAME" TEXT, "RDB$PARAMETER_NUMBER" INTEGER, "RDB$PARAMETER_TYPE" INTEGER, "RDB$FIELD_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$TRIGGERS" ("RDB$TRIGGER_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$TRIGGER_SEQUENCE" INTEGER, "RDB$TRIGGER_TYPE" INTEGER, "RDB$TRIGGER_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$TRIGGER_INACTIVE" INTEGER, "RDB$SYSTEM_FLAG" INTEGER, "RDB$FLAGS" INTEGER)`,
	`CREATE TABLE "RDB$GENERATORS" ("RDB$GENERATOR_NAME" TEXT, "RDB$GENERATOR_ID" INTEGER, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$FUNCTIONS" ("RDB$FUNCTION_NAME" TEXT, "RDB$FUNCTION_TYPE" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$MODULE_NAME" TEXT, "RDB$ENTRYPOINT" TEXT, "RDB$RETURN_ARGUMENT" INTEGER, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$FUNCTION_ARGUMENTS" ("RDB$FUNCTION_NAME" TEXT, "RDB$ARGUMENT_POSITION" INTEGER, "RDB$MECHANISM" INTEGER, "RDB$FIELD_LENGTH" INTEGER, "RDB$FIELD_SCALE" INTEGER, "RDB$FIELD_TYPE" INTEGER, "RDB$FIELD_SUB_TYPE" INTEGER, "RDB$CHARACTER_SET_ID" INTEGER, "RDB$FIELD_PRECISION" INTEGER, "RDB$CHARACTER_LENGTH" INTEGER)`,
}

func TestInterBaseSchemaFixtureFeedsTheCatalogReader(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()
	catalog := schema.New(db)

	relations, err := catalog.Relations(ctx, "")
	if err != nil {
		t.Fatalf("Relations() error = %v", err)
	}
	wantColumns := map[string]int{"CHILD": 3, "CUSTOMER": 11, "CUSTOMER_VIEW": 1, "PARENT": 2}
	if len(relations) != len(wantColumns) {
		t.Fatalf("Relations() returned %d relations, want %d (the system relation must be filtered)", len(relations), len(wantColumns))
	}
	for _, relation := range relations {
		want, ok := wantColumns[relation.Name]
		if !ok {
			t.Fatalf("unexpected relation %q", relation.Name)
		}
		// Zero columns here means the fixture padded a join key: schema
		// re-queries columns with the trimmed relation name.
		if len(relation.Columns) != want {
			t.Errorf("relation %q has %d columns, want %d", relation.Name, len(relation.Columns), want)
		}
	}

	constraints, err := catalog.Constraints(ctx, "")
	if err != nil {
		t.Fatalf("Constraints() error = %v", err)
	}
	if len(constraints) != 3 {
		t.Fatalf("Constraints() returned %d constraints, want 3", len(constraints))
	}
	foreignKey := constraints[0]
	if foreignKey.Name != "FK_CHILD" {
		t.Fatalf("first constraint = %q, want FK_CHILD (catalog order is by name)", foreignKey.Name)
	}
	if got, want := strings.Join(foreignKey.Columns, ","), "CHILD_B,CHILD_A"; got != want {
		t.Errorf("FK columns = %q, want %q", got, want)
	}
	if got, want := strings.Join(foreignKey.ReferencedColumns, ","), "PARENT_B,PARENT_A"; got != want {
		t.Errorf("FK referenced columns = %q, want %q", got, want)
	}

	// The rewrite guard: Domains is the one method whose SQL SQLite rejects.
	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		t.Fatalf("Domains() error = %v (the NOT STARTING WITH rewrite is missing or wrong)", err)
	}
	// Assert presence before absence. The loop below asserts nothing at all on
	// an empty result set, so without this the test that advertises itself as
	// the rewrite guard would pass against a fixture returning no domains.
	userDomains := map[string]bool{}
	for _, domain := range domains {
		userDomains[domain.Name] = true
	}
	for _, want := range []string{"EMAIL_ADDRESS", "CUSTOMER_CODE"} {
		if !userDomains[want] {
			t.Errorf("Domains() did not return the user domain %q; got %v", want, domains)
		}
	}
	for _, domain := range domains {
		if strings.HasPrefix(domain.Name, "RDB$") {
			t.Errorf("Domains() returned the system domain %q", domain.Name)
		}
	}

	procedures, err := catalog.Procedures(ctx, "")
	if err != nil {
		t.Fatalf("Procedures() error = %v", err)
	}
	if len(procedures) != 1 || len(procedures[0].InputParameters) != 2 || len(procedures[0].OutputParameters) != 1 {
		t.Fatalf("Procedures() = %d procedures with %d/%d parameters, want 1 with 2/1",
			len(procedures), len(procedures[0].InputParameters), len(procedures[0].OutputParameters))
	}

	functions, err := catalog.Functions(ctx, "")
	if err != nil {
		t.Fatalf("Functions() error = %v", err)
	}
	if len(functions) != 1 || len(functions[0].Arguments) != 3 {
		t.Fatalf("Functions() = %d functions with %d arguments, want 1 with 3", len(functions), len(functions[0].Arguments))
	}

	// Display-only padding must be trimmed by the reader, not by the fixture.
	if got := relations[0].OwnerName.String; got != "SYSDBA" {
		t.Errorf("relation owner = %q, want %q (catalog padding must be trimmed)", got, "SYSDBA")
	}
}

func TestInterBaseSchemaFixtureSupportsReadOnlyTransactions(t *testing.T) {
	// The snapshot capability begins a read-only transaction and keeps it open
	// while the rest of the pool stays usable. Pin that the fixture supports
	// both, so a snapshot failure later is a real defect and not the fixture.
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(ReadOnly) error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	relations, err := schema.New(tx).Relations(ctx, "")
	if err != nil {
		t.Fatalf("Relations() through a transaction error = %v", err)
	}
	if len(relations) != 4 {
		t.Fatalf("Relations() through a transaction returned %d relations, want 4", len(relations))
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "RDB$RELATIONS"`).Scan(&count); err != nil {
		t.Fatalf("a second connection must stay usable while the snapshot transaction is open: %v", err)
	}
	if count != 5 {
		t.Fatalf("second connection saw %d relations, want 5 (shared-cache memory database)", count)
	}
}

func interBaseNullInt(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

func interBaseNullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func TestInterBaseTypeRenderingByDialect(t *testing.T) {
	tests := []struct {
		name         string
		domain       *schema.Domain
		wantDialect1 string
		wantDialect3 string
	}{
		// The spec's core table.
		{
			name:         "field type 35 is the only dialect-dependent rule",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(35)},
			wantDialect1: "DATE",
			wantDialect3: "TIMESTAMP",
		},
		{
			name:         "field type 12 is DATE in both dialects",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(12)},
			wantDialect1: "DATE", wantDialect3: "DATE",
		},
		{
			name:         "field type 13 is TIME in both dialects",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(13)},
			wantDialect1: "TIME", wantDialect3: "TIME",
		},
		{
			name: "integer with a numeric subtype renders NUMERIC",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(-2), FieldPrecision: interBaseNullInt(9)},
			wantDialect1: "NUMERIC(9, 2)", wantDialect3: "NUMERIC(9, 2)",
		},
		{
			name: "scaled DOUBLE without a numeric subtype is dialect 1 fixed point",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(0),
				FieldScale: interBaseNullInt(-2)},
			wantDialect1: "NUMERIC(15, 2)", wantDialect3: "NUMERIC(15, 2)",
		},
		{
			name: "DOUBLE with subtype 2 renders DECIMAL",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(2),
				FieldScale: interBaseNullInt(-4), FieldPrecision: interBaseNullInt(18)},
			wantDialect1: "DECIMAL(18, 4)", wantDialect3: "DECIMAL(18, 4)",
		},
		{
			name: "unscaled DOUBLE stays DOUBLE PRECISION",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(0),
				FieldScale: interBaseNullInt(0)},
			wantDialect1: "DOUBLE PRECISION", wantDialect3: "DOUBLE PRECISION",
		},
		{
			name: "CHAR reports its character length without the charset suffix",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(10),
				CharacterSetID: interBaseNullInt(4), CharacterSetName: interBaseNullString(interBaseFixed("UTF8"))},
			wantDialect1: "CHAR(10)", wantDialect3: "CHAR(10)",
		},
		{
			name:         "VARCHAR reports its character length",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(37), CharacterLength: interBaseNullInt(20)},
			wantDialect1: "VARCHAR(20)", wantDialect3: "VARCHAR(20)",
		},
		{
			name:         "a text BLOB reports its subtype",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(261), FieldSubType: interBaseNullInt(1)},
			wantDialect1: "BLOB SUB_TYPE TEXT", wantDialect3: "BLOB SUB_TYPE TEXT",
		},
		{
			name:         "QUAD survives through the retained switch",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(9)},
			wantDialect1: "QUAD", wantDialect3: "QUAD",
		},
		{
			name:         "BLOB_ID survives through the retained switch",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(45)},
			wantDialect1: "BLOB_ID", wantDialect3: "BLOB_ID",
		},
		{
			name:         "CSTRING survives through the retained switch",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(40), CharacterLength: interBaseNullInt(32)},
			wantDialect1: "CSTRING(32)", wantDialect3: "CSTRING(32)",
		},
		{
			name:         "an unrecognized field type is named, not dropped",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(99)},
			wantDialect1: "TYPE(99)", wantDialect3: "TYPE(99)",
		},
		{name: "a nil domain renders nothing", domain: nil, wantDialect1: "", wantDialect3: ""},

		// Every remaining path on which Domain.SQLType() returns
		// ErrUnsupportedDDL and the retained switch must catch it. Spec §4.3.
		{
			name:         "an array falls back to its base type name",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), Dimensions: interBaseNullInt(1)},
			wantDialect1: "INTEGER", wantDialect3: "INTEGER",
		},
		{
			name: "a numeric subtype with no precision uses the natural precision",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(-2)},
			wantDialect1: "NUMERIC(9, 2)", wantDialect3: "NUMERIC(9, 2)",
		},
		{
			name: "a numeric subtype with no scale renders scale zero",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldPrecision: interBaseNullInt(9)},
			wantDialect1: "NUMERIC(9, 0)", wantDialect3: "NUMERIC(9, 0)",
		},
		{
			name:         "a positive scale on subtype 0 renders the plain base name",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldScale: interBaseNullInt(2)},
			wantDialect1: "INTEGER", wantDialect3: "INTEGER",
		},
		{
			name:         "CHAR with a NULL character length falls back to the field length",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), FieldLength: interBaseNullInt(10)},
			wantDialect1: "CHAR(10)", wantDialect3: "CHAR(10)",
		},
		{
			name: "an unavailable charset name still renders a plain CHAR",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(5),
				CharacterSetID: interBaseNullInt(4)},
			wantDialect1: "CHAR(5)", wantDialect3: "CHAR(5)",
		},
		{
			name: "an unavailable collation name still renders a plain CHAR",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(5),
				CollationID: interBaseNullInt(2)},
			wantDialect1: "CHAR(5)", wantDialect3: "CHAR(5)",
		},
		{
			name:         "an invalid field type renders nothing rather than TYPE(0)",
			domain:       &schema.Domain{Name: "D"},
			wantDialect1: "", wantDialect3: "",
		},

		// The remaining switch arms, so a later edit cannot drop one.
		{
			name:         "SMALLINT with a negative scale",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(7), FieldScale: interBaseNullInt(-1)},
			wantDialect1: "NUMERIC(4, 1)", wantDialect3: "NUMERIC(4, 1)",
		},
		{
			name: "BIGINT", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(16)},
			wantDialect1: "BIGINT", wantDialect3: "BIGINT",
		},
		{
			name: "FLOAT", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(10)},
			wantDialect1: "FLOAT", wantDialect3: "FLOAT",
		},
		{
			name: "BOOLEAN", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(17)},
			wantDialect1: "BOOLEAN", wantDialect3: "BOOLEAN",
		},

		// Two cases where SQLType() succeeds and disagrees with the old
		// switch. They are upgrades, not regressions, but they change text a
		// user sees, so they are pinned rather than discovered. See the
		// "documented divergences" note under this task.
		{
			name: "a numeric subtype with zero scale is NUMERIC, not DOUBLE PRECISION",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(0), FieldPrecision: interBaseNullInt(15)},
			wantDialect1: "NUMERIC(15, 0)", wantDialect3: "NUMERIC(15, 0)",
		},
		{
			name:         "a binary BLOB names its subtype",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(261), FieldSubType: interBaseNullInt(0)},
			wantDialect1: "BLOB SUB_TYPE BINARY", wantDialect3: "BLOB SUB_TYPE BINARY",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := interBaseColumnTypeName(test.domain, 1); got != test.wantDialect1 {
				t.Errorf("dialect 1 column type = %q, want %q", got, test.wantDialect1)
			}
			if got := interBaseColumnTypeName(test.domain, 3); got != test.wantDialect3 {
				t.Errorf("dialect 3 column type = %q, want %q", got, test.wantDialect3)
			}
			// Zero means dialect 3, matching the driver's normalizeDialect and
			// the zero value of InterBaseDBRepository.SQLDialect.
			if got := interBaseColumnTypeName(test.domain, 0); got != test.wantDialect3 {
				t.Errorf("dialect 0 column type = %q, want the dialect 3 rendering %q", got, test.wantDialect3)
			}
		})
	}
}

func TestInterBaseTypeNameKeepsCharsetAndCollationOutOfTheColumnForm(t *testing.T) {
	// The full rendering is what DomainDesc.Type and DDL carry; the trimmed
	// one is what the completion detail line carries. Both come from the same
	// renderer, so this pins the cut rather than a second code path.
	domain := &schema.Domain{
		Name:             "EMAIL_ADDRESS",
		FieldType:        interBaseNullInt(14),
		CharacterLength:  interBaseNullInt(10),
		CharacterSetID:   interBaseNullInt(4),
		CharacterSetName: interBaseNullString(interBaseFixed("UTF8")),
		CollationID:      interBaseNullInt(2),
		CollationName:    interBaseNullString(interBaseFixed("UNICODE")),
	}

	wantFull := `CHAR(10) CHARACTER SET "UTF8" COLLATE "UNICODE"`
	if got := interBaseTypeName(domain, 3); got != wantFull {
		t.Errorf("interBaseTypeName() = %q, want %q", got, wantFull)
	}
	if got := interBaseColumnTypeName(domain, 3); got != "CHAR(10)" {
		t.Errorf("interBaseColumnTypeName() = %q, want %q", got, "CHAR(10)")
	}

	charsetOnly := &schema.Domain{
		Name:             "CODE",
		FieldType:        interBaseNullInt(37),
		CharacterLength:  interBaseNullInt(20),
		CharacterSetID:   interBaseNullInt(4),
		CharacterSetName: interBaseNullString(interBaseFixed("UTF8")),
	}
	if got, want := interBaseTypeName(charsetOnly, 3), `VARCHAR(20) CHARACTER SET "UTF8"`; got != want {
		t.Errorf("interBaseTypeName() = %q, want %q", got, want)
	}
	if got := interBaseColumnTypeName(charsetOnly, 3); got != "VARCHAR(20)" {
		t.Errorf("interBaseColumnTypeName() = %q, want %q", got, "VARCHAR(20)")
	}
}

func TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	// Preserved verbatim from the pre-migration test: this repository has no
	// DatabaseName, and CurrentDatabase/Databases stay inert until plan 3
	// wires §4.7. This is the "no identity" regression guard.
	if got, err := repository.CurrentDatabase(ctx); err != nil || got != "" {
		t.Fatalf("CurrentDatabase() = (%q, %v), want (empty, nil)", got, err)
	}
	if got, err := repository.Databases(ctx); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("Databases() = (%#v, %v), want empty list", got, err)
	}
	if got, err := repository.CurrentSchema(ctx); err != nil || got != "" {
		t.Fatalf("CurrentSchema() = (%q, %v), want (empty, nil)", got, err)
	}
	if got, err := repository.Schemas(ctx); err != nil || !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("Schemas() = (%#v, %v), want synthetic empty schema", got, err)
	}

	// Views stay in SchemaTables: the extended view cache is additive
	// metadata, not a replacement, so views keep completing in FROM position.
	wantSchemaTables := map[string][]string{
		"": {"CHILD", "CUSTOMER", "CUSTOMER_VIEW", "PARENT"},
	}
	gotSchemaTables, err := repository.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if !reflect.DeepEqual(gotSchemaTables, wantSchemaTables) {
		t.Fatalf("SchemaTables() = %#v, want %#v", gotSchemaTables, wantSchemaTables)
	}

	gotColumns, err := repository.DescribeDatabaseTable(ctx)
	if err != nil {
		t.Fatalf("DescribeDatabaseTable() error = %v", err)
	}
	wantColumns := []struct {
		table, name, typ, nullable, key, extra, defaultValue string
		defaultValid                                         bool
	}{
		{table: "CHILD", name: "CHILD_B", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "CHILD", name: "CHILD_A", typ: "INTEGER", nullable: "YES", key: "NO"},
		// Kept with an empty type rather than dropped; see the named test below.
		{table: "CHILD", name: "ORPHAN", typ: "", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "ID", typ: "INTEGER", nullable: "NO", key: "YES"},
		{table: "CUSTOMER", name: "CODE", typ: "CHAR(10)", nullable: "YES", key: "NO"},
		// Documented type change: dialect 3 distinguishes TIMESTAMP from DATE.
		{table: "CUSTOMER", name: "CREATED", typ: "TIMESTAMP", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "AMOUNT", typ: "NUMERIC(9, 2)", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "LABEL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'  seeded  '", defaultValid: true},
		{table: "CUSTOMER", name: "INHERITED", typ: "VARCHAR(20)", nullable: "NO", key: "NO", defaultValue: "'domain'", defaultValid: true},
		{table: "CUSTOMER", name: "OVERRIDE", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'column'", defaultValid: true},
		{table: "CUSTOMER", name: "DEFAULT_NULL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "NULL", defaultValid: true},
		{table: "CUSTOMER", name: "REQUIRED", typ: "INTEGER", nullable: "NO", key: "NO"},
		// Documented type change: the numeric subtype is the declaration.
		{table: "CUSTOMER", name: "DOUBLE_AMOUNT", typ: "NUMERIC(15, 0)", nullable: "YES", key: "NO"},
		// New: computed columns are common and hover should say so.
		{table: "CUSTOMER", name: "TOTAL", typ: "INTEGER", nullable: "YES", key: "NO", extra: "COMPUTED"},
		{table: "CUSTOMER_VIEW", name: "VIEW_ID", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "PARENT", name: "PARENT_B", typ: "INTEGER", nullable: "YES", key: "YES"},
		{table: "PARENT", name: "PARENT_A", typ: "INTEGER", nullable: "YES", key: "YES"},
	}
	if len(gotColumns) != len(wantColumns) {
		t.Fatalf("DescribeDatabaseTable() returned %d columns, want %d", len(gotColumns), len(wantColumns))
	}
	for i, want := range wantColumns {
		got := gotColumns[i]
		if got.Schema != "" {
			t.Errorf("column %d schema = %q, want synthetic empty schema", i, got.Schema)
		}
		if got.Table != want.table || got.Name != want.name || got.Type != want.typ ||
			got.Null != want.nullable || got.Key != want.key || got.Extra != want.extra {
			t.Errorf("column %d = (%q, %q, %q, %q, %q, %q), want (%q, %q, %q, %q, %q, %q)",
				i, got.Table, got.Name, got.Type, got.Null, got.Key, got.Extra,
				want.table, want.name, want.typ, want.nullable, want.key, want.extra)
		}
		if got.Default.Valid != want.defaultValid || got.Default.String != want.defaultValue {
			t.Errorf("column %d default = %#v, want %#v", i, got.Default,
				sql.NullString{String: want.defaultValue, Valid: want.defaultValid})
		}
	}

	bySchema, err := repository.DescribeDatabaseTableBySchema(ctx, "ignored-schema")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() error = %v", err)
	}
	if len(bySchema) != len(gotColumns) {
		t.Fatalf("DescribeDatabaseTableBySchema() returned %d columns, want %d", len(bySchema), len(gotColumns))
	}
	for i := range gotColumns {
		if bySchema[i].Table != gotColumns[i].Table || bySchema[i].Name != gotColumns[i].Name {
			t.Errorf("schema column %d = %s.%s, want %s.%s", i,
				bySchema[i].Table, bySchema[i].Name, gotColumns[i].Table, gotColumns[i].Name)
		}
	}

	foreignKeys, err := repository.DescribeForeignKeysBySchema(ctx, "ignored-schema")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	if len(foreignKeys) != 1 || len(*foreignKeys[0]) != 2 {
		t.Fatalf("DescribeForeignKeysBySchema() = %#v, want one two-column foreign key", foreignKeys)
	}
	wantForeignKeyTables := [][2]string{{"CHILD", "PARENT"}, {"CHILD", "PARENT"}}
	wantForeignKeyNames := [][2]string{{"CHILD_B", "PARENT_B"}, {"CHILD_A", "PARENT_A"}}
	for i, pair := range *foreignKeys[0] {
		if pair[0].Schema != "" || pair[1].Schema != "" ||
			pair[0].Table != wantForeignKeyTables[i][0] || pair[1].Table != wantForeignKeyTables[i][1] ||
			pair[0].Name != wantForeignKeyNames[i][0] || pair[1].Name != wantForeignKeyNames[i][1] {
			t.Errorf("foreign key pair %d = %#v, want %s.%s -> %s.%s", i, pair,
				wantForeignKeyTables[i][0], wantForeignKeyNames[i][0],
				wantForeignKeyTables[i][1], wantForeignKeyNames[i][1])
		}
	}
}

func TestInterBaseColumnTypesFollowTheRepositoryDialect(t *testing.T) {
	// The repository's SQLDialect must actually reach the renderer. Plan 1
	// populates it from the resolved connection variant; a zero value means
	// dialect 3, matching interbase-go's own normalizeDialect.
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()

	tests := []struct {
		sqlDialect int
		want       string
	}{
		{sqlDialect: 0, want: "TIMESTAMP"},
		{sqlDialect: 1, want: "DATE"},
		{sqlDialect: 3, want: "TIMESTAMP"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("dialect %d", test.sqlDialect), func(t *testing.T) {
			repository := &InterBaseDBRepository{Conn: db, SQLDialect: test.sqlDialect}
			columns, err := repository.DescribeDatabaseTable(ctx)
			if err != nil {
				t.Fatalf("DescribeDatabaseTable() error = %v", err)
			}
			for _, column := range columns {
				if column.Table == "CUSTOMER" && column.Name == "CREATED" {
					if column.Type != test.want {
						t.Fatalf("CUSTOMER.CREATED type = %q, want %q", column.Type, test.want)
					}
					return
				}
			}
			t.Fatal("CUSTOMER.CREATED was not returned")
		})
	}
}

func TestInterBaseColumnWithoutDomainRowIsRetained(t *testing.T) {
	// A deliberate, asserted behavior change. The old inner JOIN RDB$FIELDS
	// (interbase_common.go:195-196) silently dropped a column whose
	// RDB$FIELD_SOURCE had no RDB$FIELDS row; schema LEFT JOINs and yields
	// Domain == nil. Showing a column with an unknown type beats hiding a
	// column that exists.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	columns, err := repository.DescribeDatabaseTable(context.Background())
	if err != nil {
		t.Fatalf("DescribeDatabaseTable() error = %v", err)
	}

	var orphan *ColumnDesc
	for _, column := range columns {
		if column.Table == "CHILD" && column.Name == "ORPHAN" {
			orphan = column
		}
	}
	if orphan == nil {
		t.Fatal("a column whose field source has no RDB$FIELDS row was dropped; it must be kept with an empty type")
	}
	if orphan.Type != "" {
		t.Errorf("orphan column type = %q, want an empty type", orphan.Type)
	}
	if orphan.Null != "YES" || orphan.Key != "NO" || orphan.Default.Valid {
		t.Errorf("orphan column = %#v, want nullable, non-key, no default", orphan)
	}
}

func TestInterBaseDescribesViewsGeneratorsDomainsAndIndexes(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	views, err := repository.DescribeViews(ctx)
	if err != nil {
		t.Fatalf("DescribeViews() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("DescribeViews() returned %d views, want 1", len(views))
	}
	view := views[0]
	if view.Schema != "" || view.Name != "CUSTOMER_VIEW" {
		t.Errorf("view identity = (%q, %q), want (\"\", \"CUSTOMER_VIEW\")", view.Schema, view.Name)
	}
	if got, want := view.OwnerName.String, "SYSDBA"; !view.OwnerName.Valid || got != want {
		t.Errorf("view owner = %#v, want %q", view.OwnerName, want)
	}
	if got, want := view.ViewSource.String, "SELECT ID FROM CUSTOMER"; !view.ViewSource.Valid || got != want {
		t.Errorf("view source = %#v, want %q", view.ViewSource, want)
	}
	if len(view.Columns) != 1 || view.Columns[0].Name != "VIEW_ID" || view.Columns[0].Type != "INTEGER" {
		t.Fatalf("view columns = %#v, want one INTEGER VIEW_ID", view.Columns)
	}
	if view.Columns[0].Table != "CUSTOMER_VIEW" {
		t.Errorf("view column table = %q, want %q", view.Columns[0].Table, "CUSTOMER_VIEW")
	}

	generators, err := repository.DescribeGenerators(ctx)
	if err != nil {
		t.Fatalf("DescribeGenerators() error = %v", err)
	}
	if len(generators) != 1 || generators[0].Name != "GEN_CUSTOMER_ID" {
		t.Fatalf("DescribeGenerators() = %#v, want one GEN_CUSTOMER_ID", generators)
	}
	if !generators[0].ID.Valid || generators[0].ID.Int64 != 1 {
		t.Errorf("generator id = %#v, want 1", generators[0].ID)
	}

	domains, err := repository.DescribeDomains(ctx)
	if err != nil {
		t.Fatalf("DescribeDomains() error = %v", err)
	}
	byName := make(map[string]*DomainDesc, len(domains))
	for _, domain := range domains {
		if strings.HasPrefix(domain.Name, "RDB$") {
			t.Errorf("DescribeDomains() returned the system domain %q", domain.Name)
		}
		byName[domain.Name] = domain
	}

	email, ok := byName["EMAIL_ADDRESS"]
	if !ok {
		t.Fatalf("DescribeDomains() did not return EMAIL_ADDRESS: %v", byName)
	}
	if email.Type != "VARCHAR(100)" {
		t.Errorf("EMAIL_ADDRESS type = %q, want %q", email.Type, "VARCHAR(100)")
	}
	if !email.Nullable.Valid || email.Nullable.Bool {
		t.Errorf("EMAIL_ADDRESS nullable = %#v, want a valid false", email.Nullable)
	}
	if got, want := email.DefaultSource.String, " DEFAULT 'a@b' "; email.DefaultSource.String != want {
		t.Errorf("EMAIL_ADDRESS default = %q, want the verbatim catalog text %q", got, want)
	}
	if got, want := email.ValidationSource.String, "CHECK (VALUE LIKE '%@%')"; got != want {
		t.Errorf("EMAIL_ADDRESS validation = %q, want %q", got, want)
	}

	// DomainDesc.Type keeps the charset and collation suffix that
	// ColumnDesc.Type drops, and the charset/collation are separate fields.
	code, ok := byName["CUSTOMER_CODE"]
	if !ok {
		t.Fatalf("DescribeDomains() did not return CUSTOMER_CODE")
	}
	if got, want := code.Type, `CHAR(10) CHARACTER SET "UTF8" COLLATE "UNICODE"`; got != want {
		t.Errorf("CUSTOMER_CODE type = %q, want the full rendering %q", got, want)
	}
	if got, want := code.CharacterSetName.String, "UTF8"; !code.CharacterSetName.Valid || got != want {
		t.Errorf("CUSTOMER_CODE charset = %#v, want %q", code.CharacterSetName, want)
	}
	if got, want := code.CollationName.String, "UNICODE"; !code.CollationName.Valid || got != want {
		t.Errorf("CUSTOMER_CODE collation = %#v, want %q", code.CollationName, want)
	}
	if byName["CUSTOMER_LABEL"].Nullable.Valid {
		t.Errorf("CUSTOMER_LABEL nullable = %#v, want an invalid NullBool when RDB$NULL_FLAG is NULL",
			byName["CUSTOMER_LABEL"].Nullable)
	}

	indexes, err := repository.DescribeIndexes(ctx)
	if err != nil {
		t.Fatalf("DescribeIndexes() error = %v", err)
	}
	indexByName := make(map[string]*IndexDesc, len(indexes))
	for _, index := range indexes {
		indexByName[index.Name] = index
	}
	if len(indexes) != 4 {
		t.Fatalf("DescribeIndexes() returned %d indexes, want 4: %v", len(indexes), indexByName)
	}

	primaryKey := indexByName["IDX_PARENT_PK"]
	if primaryKey == nil {
		t.Fatal("DescribeIndexes() did not return IDX_PARENT_PK")
	}
	if primaryKey.RelationName != "PARENT" {
		t.Errorf("IDX_PARENT_PK relation = %q, want %q", primaryKey.RelationName, "PARENT")
	}
	if got, want := strings.Join(primaryKey.Columns, ","), "PARENT_B,PARENT_A"; got != want {
		t.Errorf("IDX_PARENT_PK segments = %q, want them ordered %q", got, want)
	}
	if !primaryKey.Unique.Valid || !primaryKey.Unique.Bool {
		t.Errorf("IDX_PARENT_PK unique = %#v, want a valid true", primaryKey.Unique)
	}
	if !primaryKey.Active.Valid || !primaryKey.Active.Bool {
		t.Errorf("IDX_PARENT_PK active = %#v, want a valid true", primaryKey.Active)
	}
	if got, want := primaryKey.ConstraintName.String, "PK_PARENT"; !primaryKey.ConstraintName.Valid || got != want {
		t.Errorf("IDX_PARENT_PK constraint = %#v, want %q", primaryKey.ConstraintName, want)
	}
	if primaryKey.Expression.Valid {
		t.Errorf("IDX_PARENT_PK expression = %#v, want invalid for a segment index", primaryKey.Expression)
	}

	// RDB$INDEX_INACTIVE is an INACTIVE flag; Active is its inverse, and a
	// standalone index has no owning constraint.
	inactive := indexByName["IDX_CUSTOMER_CODE"]
	if inactive == nil {
		t.Fatal("DescribeIndexes() did not return IDX_CUSTOMER_CODE")
	}
	if !inactive.Active.Valid || inactive.Active.Bool {
		t.Errorf("IDX_CUSTOMER_CODE active = %#v, want a valid false", inactive.Active)
	}
	if inactive.ConstraintName.Valid && inactive.ConstraintName.String != "" {
		t.Errorf("IDX_CUSTOMER_CODE constraint = %#v, want invalid for a standalone index", inactive.ConstraintName)
	}
	if !indexByName["IDX_CHILD_FK"].Unique.Valid || indexByName["IDX_CHILD_FK"].Unique.Bool {
		t.Errorf("IDX_CHILD_FK unique = %#v, want a valid false", indexByName["IDX_CHILD_FK"].Unique)
	}
}

func TestInterBaseDescribesProceduresTriggersAndFunctions(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	procedures, err := repository.DescribeProcedures(ctx)
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	if len(procedures) != 1 {
		t.Fatalf("DescribeProcedures() returned %d procedures, want 1", len(procedures))
	}
	procedure := procedures[0]
	if procedure.Schema != "" || procedure.Name != "ADD_CUSTOMER" {
		t.Errorf("procedure identity = (%q, %q), want (\"\", \"ADD_CUSTOMER\")", procedure.Schema, procedure.Name)
	}
	if got, want := procedure.OwnerName.String, "SYSDBA"; !procedure.OwnerName.Valid || got != want {
		t.Errorf("procedure owner = %#v, want %q", procedure.OwnerName, want)
	}
	if got, want := procedure.Source.String, "BEGIN NEW_ID = 1; END"; !procedure.Source.Valid || got != want {
		t.Errorf("procedure source = %#v, want the verbatim PSQL body %q", procedure.Source, want)
	}
	if len(procedure.InputParameters) != 2 || len(procedure.OutputParameters) != 1 {
		t.Fatalf("procedure has %d input and %d output parameters, want 2 and 1",
			len(procedure.InputParameters), len(procedure.OutputParameters))
	}

	wantInputs := []struct {
		name     string
		position int
		typ      string
		domain   string
		nullable sql.NullBool
	}{
		{name: "EMAIL", position: 0, typ: "VARCHAR(100)", domain: "EMAIL_ADDRESS", nullable: sql.NullBool{Bool: false, Valid: true}},
		{name: "CODE", position: 1, typ: "INTEGER", domain: "", nullable: sql.NullBool{}},
	}
	for i, want := range wantInputs {
		got := procedure.InputParameters[i]
		if got.Name != want.name || got.Position != want.position || got.Type != want.typ || got.Domain != want.domain {
			t.Errorf("input parameter %d = (%q, %d, %q, %q), want (%q, %d, %q, %q)",
				i, got.Name, got.Position, got.Type, got.Domain, want.name, want.position, want.typ, want.domain)
		}
		if got.Direction != ParameterInput {
			t.Errorf("input parameter %d direction = %q, want %q", i, got.Direction, ParameterInput)
		}
		if got.Nullable != want.nullable {
			t.Errorf("input parameter %d nullable = %#v, want %#v", i, got.Nullable, want.nullable)
		}
	}

	output := procedure.OutputParameters[0]
	if output.Name != "NEW_ID" || output.Direction != ParameterOutput || output.Type != "INTEGER" {
		t.Errorf("output parameter = (%q, %q, %q), want (\"NEW_ID\", %q, \"INTEGER\")",
			output.Name, output.Direction, output.Type, ParameterOutput)
	}
	if output.Domain != "CUSTOMER_ID" {
		t.Errorf("output parameter domain = %q, want %q", output.Domain, "CUSTOMER_ID")
	}

	triggers, err := repository.DescribeTriggers(ctx)
	if err != nil {
		t.Fatalf("DescribeTriggers() error = %v", err)
	}
	triggerByName := make(map[string]*TriggerDesc, len(triggers))
	for _, trigger := range triggers {
		triggerByName[trigger.Name] = trigger
	}
	if len(triggers) != 4 {
		t.Fatalf("DescribeTriggers() returned %d triggers, want 4: %v", len(triggers), triggerByName)
	}

	before := triggerByName["CUSTOMER_BI"]
	if before == nil {
		t.Fatal("DescribeTriggers() did not return CUSTOMER_BI")
	}
	if got, want := before.RelationName.String, "CUSTOMER"; !before.RelationName.Valid || got != want {
		t.Errorf("CUSTOMER_BI relation = %#v, want %q", before.RelationName, want)
	}
	if !before.Active.Valid || !before.Active.Bool {
		t.Errorf("CUSTOMER_BI active = %#v, want a valid true", before.Active)
	}
	if got, want := before.Source.String, "AS BEGIN END"; !before.Source.Valid || got != want {
		t.Errorf("CUSTOMER_BI source = %#v, want %q", before.Source, want)
	}
	if !before.Sequence.Valid || before.Sequence.Int64 != 0 {
		t.Errorf("CUSTOMER_BI sequence = %#v, want 0", before.Sequence)
	}

	// RDB$TRIGGER_INACTIVE is an INACTIVE flag, so Active is its inverse.
	if multi := triggerByName["CUSTOMER_MULTI"]; multi == nil || !multi.Active.Valid || multi.Active.Bool {
		t.Errorf("CUSTOMER_MULTI active = %#v, want a valid false", multi)
	}
	// A database-level trigger has no relation.
	if database := triggerByName["DB_CONNECT"]; database == nil || database.RelationName.Valid {
		t.Errorf("DB_CONNECT relation = %#v, want invalid for a database-level trigger", database)
	}

	functions, err := repository.DescribeFunctions(ctx)
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("DescribeFunctions() returned %d functions, want 1", len(functions))
	}
	function := functions[0]
	if function.Name != "F_LTRIM" {
		t.Errorf("function name = %q, want %q", function.Name, "F_LTRIM")
	}
	if got, want := function.ModuleName.String, "ib_udf"; !function.ModuleName.Valid || got != want {
		t.Errorf("function module = %#v, want %q", function.ModuleName, want)
	}
	if got, want := function.EntryPoint.String, "IB_LTRIM"; !function.EntryPoint.Valid || got != want {
		t.Errorf("function entry point = %#v, want %q", function.EntryPoint, want)
	}
	if !function.ReturnPosition.Valid || function.ReturnPosition.Int64 != 1 {
		t.Errorf("function return position = %#v, want 1", function.ReturnPosition)
	}
	// The return argument IS input argument 1, and must still appear exactly
	// once in Arguments rather than being lifted out of the list.
	if len(function.Arguments) != 3 {
		t.Fatalf("function has %d arguments, want 3", len(function.Arguments))
	}
	positions := make([]int64, 0, len(function.Arguments))
	for _, argument := range function.Arguments {
		if !argument.Position.Valid {
			t.Fatalf("argument %q has no position", argument.Name)
		}
		positions = append(positions, argument.Position.Int64)
	}
	if !reflect.DeepEqual(positions, []int64{1, 2, 3}) {
		t.Errorf("argument positions = %v, want [1 2 3] in catalog order", positions)
	}
}

func TestInterBaseProcedureParameterDomainIsUserOnly(t *testing.T) {
	// The two-condition test from §4.4: a parameter declared with an inline
	// type gets a system-generated RDB$ domain, which must never be shown as a
	// domain reference. When Domain is "", Type carries the inline rendering.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	procedures, err := repository.DescribeProcedures(context.Background())
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	parameters := procedures[0].InputParameters

	if got, want := parameters[0].Domain, "EMAIL_ADDRESS"; got != want {
		t.Errorf("a user domain must be reported: Domain = %q, want %q", got, want)
	}
	if got, want := parameters[1].Domain, ""; got != want {
		t.Errorf("an RDB$-prefixed system domain must not be reported: Domain = %q, want %q", got, want)
	}
	if got, want := parameters[1].Type, "INTEGER"; got != want {
		t.Errorf("an inline parameter must still render its type: Type = %q, want %q", got, want)
	}

	// The second condition, exercised directly because the fixture's system
	// domain is caught by the name rule first: a user-named domain flagged as
	// a system object is equally not a domain reference.
	systemFlagged := &schema.Domain{Name: "LEGACY_FLAG", SystemFlag: interBaseNullInt(1)}
	if got := interBaseUserDomainName(interBaseNullString(interBaseFixed("LEGACY_FLAG")), systemFlagged); got != "" {
		t.Errorf("a system-flagged domain must not be reported: got %q, want an empty string", got)
	}
	userFlagged := &schema.Domain{Name: "LEGACY_FLAG", SystemFlag: interBaseNullInt(0)}
	if got, want := interBaseUserDomainName(interBaseNullString(interBaseFixed("LEGACY_FLAG")), userFlagged), "LEGACY_FLAG"; got != want {
		t.Errorf("a user domain with SystemFlag 0 must be reported: got %q, want %q", got, want)
	}
	nullFlagged := &schema.Domain{Name: "LEGACY_FLAG"}
	if got, want := interBaseUserDomainName(interBaseNullString("LEGACY_FLAG"), nullFlagged), "LEGACY_FLAG"; got != want {
		t.Errorf("a user domain with a NULL SystemFlag must be reported: got %q, want %q", got, want)
	}
	if got := interBaseUserDomainName(interBaseNullString("rdb$99"), nil); got != "" {
		t.Errorf("the RDB$ prefix test must be case insensitive: got %q, want an empty string", got)
	}
	if got := interBaseUserDomainName(sql.NullString{}, nil); got != "" {
		t.Errorf("a NULL field source must report no domain: got %q", got)
	}
}

func TestInterBaseTriggerEventIsVerbatim(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	triggers, err := repository.DescribeTriggers(context.Background())
	if err != nil {
		t.Fatalf("DescribeTriggers() error = %v", err)
	}
	byName := make(map[string]*TriggerDesc, len(triggers))
	for _, trigger := range triggers {
		byName[trigger.Name] = trigger
	}

	if got, want := byName["CUSTOMER_BI"].Event, "BEFORE INSERT"; got != want {
		t.Errorf("CUSTOMER_BI event = %q, want %q", got, want)
	}
	// A multi-event trigger yields one joined string. sqls does not split it,
	// does not re-decode it, and specifies no parsing of it; a consumer that
	// wants the parts splits on " OR ".
	if got, want := byName["CUSTOMER_MULTI"].Event, "BEFORE INSERT OR UPDATE"; got != want {
		t.Errorf("CUSTOMER_MULTI event = %q, want the joined string %q", got, want)
	}

	// An undecodable trigger type and a NULL trigger type both yield "" with
	// the rest of the descriptor intact: ErrUnsupportedDDL from these
	// accessors is a normal result, not a failure.
	for _, name := range []string{"CUSTOMER_ODD", "DB_CONNECT"} {
		trigger := byName[name]
		if trigger == nil {
			t.Fatalf("DescribeTriggers() did not return %s", name)
		}
		if trigger.Event != "" {
			t.Errorf("%s event = %q, want an empty string", name, trigger.Event)
		}
		if trigger.Name != name || !trigger.Source.Valid || !trigger.Active.Valid {
			t.Errorf("%s lost surrounding metadata: %#v", name, trigger)
		}
	}
	if len(triggers) != 4 {
		t.Errorf("DescribeTriggers() returned %d triggers, want 4: an undecodable event must not drop the trigger", len(triggers))
	}
}

func TestInterBaseFunctionArgumentTypeDegradesToEmpty(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	// DescribeFunctions must return a nil error: the degradation rule is
	// asserted, not assumed.
	functions, err := repository.DescribeFunctions(context.Background())
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v, want nil despite an unrenderable argument", err)
	}
	if len(functions) != 1 {
		t.Fatalf("DescribeFunctions() returned %d functions, want 1", len(functions))
	}
	function := functions[0]

	byPosition := make(map[int64]*FunctionArgumentDesc, len(function.Arguments))
	for _, argument := range function.Arguments {
		byPosition[argument.Position.Int64] = argument
	}
	if len(byPosition) != 3 {
		t.Fatalf("function has %d arguments, want 3: an unrenderable argument must not be dropped", len(byPosition))
	}

	// CSTRING renders from RDB$FIELD_LENGTH, which is the shape every measured
	// production row has: of 357 arguments across three production databases,
	// 166 were CSTRING and RDB$CHARACTER_LENGTH was NULL in all 357.
	if got, want := byPosition[1].Type, "CSTRING(255)"; got != want {
		t.Errorf("CSTRING argument type = %q, want %q", got, want)
	}
	// CHAR and VARCHAR arguments render "" permanently, not just until the
	// accessors land: RDB$CHARACTER_LENGTH is never populated for function
	// arguments, so the renderer has no length to declare. Measured exposure:
	// 1 CHAR and 0 VARCHAR arguments in 357.
	if got := byPosition[2].Type; got != "" {
		t.Errorf("CHAR argument type = %q, want an empty string: no character length is available", got)
	}
	if got, want := byPosition[3].Type, "INTEGER"; got != want {
		t.Errorf("INTEGER argument type = %q, want %q", got, want)
	}

	// The surrounding descriptor is still fully populated.
	if function.Name != "F_LTRIM" || !function.ModuleName.Valid || !function.EntryPoint.Valid {
		t.Errorf("the surrounding FunctionDesc lost metadata: %#v", function)
	}
	if byPosition[2].Name == "" {
		t.Error("the unrenderable argument lost its name")
	}
}

func TestInterBaseFunctionReturnTypeUsesDriverResolution(t *testing.T) {
	// RDB$RETURN_ARGUMENT holds an argument POSITION, not an index into
	// Arguments. The fixture's return argument is position 1, which is also an
	// input argument, so the same argument legitimately appears both in
	// Arguments and as ReturnType. This guards against indexing Arguments by
	// ReturnArgument and against lifting the return out of the list.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	functions, err := repository.DescribeFunctions(context.Background())
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v", err)
	}
	function := functions[0]

	if !function.ReturnPosition.Valid || function.ReturnPosition.Int64 != 1 {
		t.Fatalf("return position = %#v, want 1", function.ReturnPosition)
	}
	if got, want := function.ReturnType, "CSTRING(255)"; got != want {
		t.Errorf("return type = %q, want the rendering of argument 1, %q", got, want)
	}

	// Indexing Arguments[1] would have picked position 2, the CHAR argument,
	// and produced "" here — so this assertion is what distinguishes the two
	// implementations.
	appearances := 0
	for _, argument := range function.Arguments {
		if argument.Position.Valid && argument.Position.Int64 == 1 {
			appearances++
		}
	}
	if appearances != 1 {
		t.Errorf("the return argument appears %d times in Arguments, want exactly 1", appearances)
	}
}

func TestInterBaseObjectDDL(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	rendered := []struct {
		name     string
		kind     ObjectKind
		object   string
		contains []string
	}{
		{
			name: "a plain table", kind: ObjectKindTable, object: "PARENT",
			contains: []string{`CREATE TABLE "PARENT"`, `"PARENT_B"`, `CONSTRAINT "PK_PARENT" PRIMARY KEY`},
		},
		{
			name: "a view", kind: ObjectKindView, object: "CUSTOMER_VIEW",
			contains: []string{`CREATE VIEW "CUSTOMER_VIEW"`, "SELECT ID FROM CUSTOMER"},
		},
		{
			name: "a generator uses InterBase's own keyword", kind: ObjectKindGenerator, object: "GEN_CUSTOMER_ID",
			contains: []string{`CREATE GENERATOR "GEN_CUSTOMER_ID"`},
		},
		{
			name: "a domain", kind: ObjectKindDomain, object: "EMAIL_ADDRESS",
			contains: []string{`CREATE DOMAIN "EMAIL_ADDRESS" AS VARCHAR(100)`, "CHECK (VALUE LIKE '%@%')"},
		},
		{
			name: "a trigger", kind: ObjectKindTrigger, object: "CUSTOMER_BI",
			contains: []string{`CREATE TRIGGER "CUSTOMER_BI" FOR "CUSTOMER"`, "BEFORE INSERT"},
		},
		{
			name: "an index", kind: ObjectKindIndex, object: "IDX_PARENT_PK",
			contains: []string{`CREATE UNIQUE ASCENDING INDEX "IDX_PARENT_PK" ON "PARENT"`, `"PARENT_B", "PARENT_A"`},
		},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, test.kind, test.object)
			if err != nil {
				t.Fatalf("ObjectDDL(%s, %q) error = %v", test.kind, test.object, err)
			}
			for _, want := range test.contains {
				if !strings.Contains(ddl, want) {
					t.Errorf("ObjectDDL(%s, %q) = %q, want it to contain %q", test.kind, test.object, ddl, want)
				}
			}
		})
	}

	unsupported := []struct {
		name        string
		kind        ObjectKind
		object      string
		wantObject  string
		wantFeature string
	}{
		{
			// Computed columns block table DDL, and they are common in the
			// target database, so this is the normal case rather than an edge.
			name: "a table with a computed column", kind: ObjectKindTable, object: "CUSTOMER",
			wantObject: "table", wantFeature: "TOTAL",
		},
		{
			// schema/README.md: procedure parameter nullability is unknown
			// unless a non-nullable domain proves it.
			name: "a procedure with unknown parameter nullability", kind: ObjectKindProcedure, object: "ADD_CUSTOMER",
			wantObject: "procedure", wantFeature: "CODE",
		},
		{
			name: "an external function is never reproducible", kind: ObjectKindFunction, object: "F_LTRIM",
			wantObject: "external function", wantFeature: "external calling convention",
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, test.kind, test.object)
			if !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("ObjectDDL(%s, %q) error = %v, want it to satisfy errors.Is(err, ErrUnsupportedDDL)",
					test.kind, test.object, err)
			}
			if errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("an existing object must not report ErrObjectNotFound: %v", err)
			}
			if ddl != "" {
				t.Errorf("ObjectDDL(%s, %q) = %q, want no text alongside the error", test.kind, test.object, ddl)
			}
			// Structural, not a substring match on the message: sub-project 3
			// renders the reason from these three fields.
			object, name, feature, ok := UnsupportedDDLDetail(err)
			if !ok {
				t.Fatalf("UnsupportedDDLDetail(%v) reported ok == false, want the structured reason", err)
			}
			if object != test.wantObject {
				t.Errorf("detail object = %q, want %q", object, test.wantObject)
			}
			if name != test.object {
				t.Errorf("detail name = %q, want %q", name, test.object)
			}
			if !strings.Contains(feature, test.wantFeature) {
				t.Errorf("detail feature = %q, want it to name %q", feature, test.wantFeature)
			}
		})
	}

	// "No such object" and "the object exists but has no renderable DDL" are
	// the one distinction sub-project 3 needs in order to choose between
	// showing nothing and showing a reason, so ("", nil) is never returned.
	kinds := []ObjectKind{
		ObjectKindTable, ObjectKindView, ObjectKindProcedure, ObjectKindTrigger,
		ObjectKindDomain, ObjectKindIndex, ObjectKindGenerator, ObjectKindFunction,
	}
	for _, kind := range kinds {
		t.Run("unknown "+string(kind), func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, kind, "NO_SUCH_OBJECT")
			if !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("ObjectDDL(%s, unknown) error = %v, want ErrObjectNotFound", kind, err)
			}
			if ddl != "" {
				t.Errorf("ObjectDDL(%s, unknown) = %q, want no text", kind, ddl)
			}
		})
	}

	if _, err := repository.ObjectDDL(ctx, ObjectKindTable, ""); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("ObjectDDL with an empty name error = %v, want ErrObjectNotFound", err)
	}
	_, err := repository.ObjectDDL(ctx, ObjectKind("nonsense"), "PARENT")
	if err == nil {
		t.Fatal("ObjectDDL with an unknown kind returned a nil error")
	}
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrUnsupportedDDL) {
		t.Errorf("an unknown object kind is a caller bug, not a catalog outcome: %v", err)
	}
}

func TestInterBaseObjectDDLDoesNotNormalizeNames(t *testing.T) {
	// schema matches catalog names exactly and does not case-fold
	// (Catalog.Table(ctx, "CUSTOMER   ") returns nil for padding; the same
	// holds for case). ObjectDDL passes the caller's name through unchanged
	// so a genuinely lower-case delimited identifier stays reachable -
	// folding to upper case here would make such an identifier permanently
	// unreachable. Normalization happens once, in DBCache (a later task),
	// before a caller reaches ObjectDDL with the resolved descriptor's own
	// catalog spelling.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	_, err := repository.ObjectDDL(ctx, ObjectKindTable, "parent")
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("ObjectDDL(table, %q) error = %v, want ErrObjectNotFound: the catalog stores %q and a lower-case name must not match it",
			"parent", err, "PARENT")
	}
}

func TestInterBaseUnsupportedDDLWithoutDetailStillMatchesTheSentinel(t *testing.T) {
	// The driver may return a bare schema.ErrUnsupportedDDL with no structured
	// reason. errors.Is must still hold; UnsupportedDDLDetail reports ok ==
	// false and sub-project 3 degrades to a generic line.
	wrapped := interBaseWrapDDLError(schema.ErrUnsupportedDDL)
	if !errors.Is(wrapped, ErrUnsupportedDDL) {
		t.Fatalf("interBaseWrapDDLError(bare sentinel) = %v, want it to satisfy errors.Is(err, ErrUnsupportedDDL)", wrapped)
	}
	if _, _, _, ok := UnsupportedDDLDetail(wrapped); ok {
		t.Error("a bare sentinel must report ok == false")
	}

	// An unrelated error passes through untouched: a real catalog fault must
	// not be relabelled as an unsupported-DDL outcome.
	fault := errors.New("interbase: connection reset")
	if got := interBaseWrapDDLError(fault); !errors.Is(got, fault) {
		t.Errorf("interBaseWrapDDLError(other) = %v, want the original error", got)
	}
	if errors.Is(interBaseWrapDDLError(fault), ErrUnsupportedDDL) {
		t.Error("an unrelated error must not satisfy errors.Is(err, ErrUnsupportedDDL)")
	}
	if interBaseWrapDDLError(nil) != nil {
		t.Error("interBaseWrapDDLError(nil) must be nil")
	}
}

func TestInterBaseCatalogSnapshotServesReadsFromOneTransaction(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3, DatabaseName: "/srv/interbase/example.ib"}
	ctx := context.Background()

	snapshot, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if closeSnapshot == nil {
		t.Fatal("CatalogSnapshot() returned a nil closer")
	}

	// A new repository, not the receiver: ReCache runs on a handler goroutine
	// while the worker's secondary pass runs on its own, so a shared mutable
	// snapshot field would race.
	if snapshot == DBRepository(source) {
		t.Fatal("CatalogSnapshot() returned the source repository; it must return a new one")
	}
	if source.snapshot != nil {
		t.Fatal("CatalogSnapshot() mutated the source repository")
	}
	bound, ok := snapshot.(*InterBaseDBRepository)
	if !ok {
		t.Fatalf("CatalogSnapshot() = %T, want *InterBaseDBRepository", snapshot)
	}
	if bound.snapshot == nil {
		t.Fatal("the returned repository is not bound to a snapshot")
	}
	if bound.SQLDialect != source.SQLDialect || bound.DatabaseName != source.DatabaseName {
		t.Errorf("snapshot repository = (%d, %q), want the source's (%d, %q)",
			bound.SQLDialect, bound.DatabaseName, source.SQLDialect, source.DatabaseName)
	}

	// The snapshot serves the same answers as the direct path.
	directTables, err := source.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() on the source error = %v", err)
	}
	snapshotTables, err := snapshot.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() on the snapshot error = %v", err)
	}
	if !reflect.DeepEqual(directTables, snapshotTables) {
		t.Errorf("snapshot SchemaTables() = %#v, want the direct result %#v", snapshotTables, directTables)
	}

	directColumns, err := source.DescribeDatabaseTableBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() on the source error = %v", err)
	}
	snapshotColumns, err := snapshot.DescribeDatabaseTableBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() on the snapshot error = %v", err)
	}
	if len(snapshotColumns) != len(directColumns) {
		t.Fatalf("snapshot returned %d columns, want %d", len(snapshotColumns), len(directColumns))
	}

	foreignKeys, err := snapshot.DescribeForeignKeysBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() on the snapshot error = %v", err)
	}
	if len(foreignKeys) != 1 {
		t.Errorf("snapshot returned %d foreign keys, want 1", len(foreignKeys))
	}

	// Extended-catalog reads run on the same transaction.
	if views, err := bound.DescribeViews(ctx); err != nil || len(views) != 1 {
		t.Errorf("DescribeViews() on the snapshot = (%d views, %v), want (1, nil)", len(views), err)
	}

	// The source repository is unaffected and remains usable concurrently.
	if _, err := source.DescribeViews(ctx); err != nil {
		t.Errorf("the source repository must stay usable while a snapshot is open: %v", err)
	}

	if err := closeSnapshot(); err != nil {
		t.Errorf("close() error = %v", err)
	}
	if _, err := source.SchemaTables(ctx); err != nil {
		t.Errorf("the source repository must stay usable after the snapshot closes: %v", err)
	}
}

func TestInterBaseCatalogSnapshotHalvesThePerRelationReads(t *testing.T) {
	// The reason CatalogSnapshot exists is round-trip count, so count them
	// rather than asserting the structure and trusting the arithmetic. The
	// fixture's driver funnels every statement through PrepareContext, so the
	// counter is the exact number of statements a cache build issues.
	db := openInterBaseSchemaFixture(t)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	// The direct path: SchemaTables and DescribeDatabaseTableBySchema each walk
	// every relation, because Relations issues one column query per relation.
	prepares := interBaseFixtureCountPrepares(t)
	if _, err := repo.SchemaTables(ctx); err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if _, err := repo.DescribeDatabaseTableBySchema(ctx, ""); err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() error = %v", err)
	}
	if _, err := repo.DescribeForeignKeysBySchema(ctx, ""); err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	direct := prepares()

	// The snapshot path: one Relations read shared by both.
	prepares = interBaseFixtureCountPrepares(t)
	snapshot, closeSnapshot, err := repo.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if _, err := snapshot.SchemaTables(ctx); err != nil {
		t.Fatalf("snapshot SchemaTables() error = %v", err)
	}
	if _, err := snapshot.DescribeDatabaseTableBySchema(ctx, ""); err != nil {
		t.Fatalf("snapshot DescribeDatabaseTableBySchema() error = %v", err)
	}
	if _, err := snapshot.DescribeForeignKeysBySchema(ctx, ""); err != nil {
		t.Fatalf("snapshot DescribeForeignKeysBySchema() error = %v", err)
	}
	if err := closeSnapshot(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	snapshotted := prepares()

	// Do not pin an exact number: it moves whenever the fixture gains a
	// relation or a constraint, and the claim is about growth, not a constant.
	// The per-relation term is what halves, so the snapshot path must issue
	// strictly fewer statements than the direct path for the same answers.
	if snapshotted >= direct {
		t.Errorf("snapshot issued %d statements, direct issued %d; the snapshot must share one relation read",
			snapshotted, direct)
	}
	t.Logf("catalog reads: direct %d statements, snapshot %d", direct, snapshotted)
}

func TestInterBaseCatalogSnapshotCloseIsSafeToCallTwice(t *testing.T) {
	// The close contract is a resource release: a caller that calls it twice
	// (e.g. once explicitly and once via a deferred close) must not panic or
	// corrupt the repository. The second call is allowed to report an error
	// (the transaction is already gone), but it must be a clean error return.
	db := openInterBaseSchemaFixture(t)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	_, closeSnapshot, err := repo.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if err := closeSnapshot(); err != nil {
		t.Fatalf("first close() error = %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second close() panicked: %v", r)
			}
		}()
		_ = closeSnapshot()
	}()

	if _, err := repo.SchemaTables(ctx); err != nil {
		t.Errorf("the source repository must stay usable after a double close: %v", err)
	}
}
