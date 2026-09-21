package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
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
