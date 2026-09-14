package database

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestInterBaseCatalogRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys(t *testing.T) {
	db := openInterBaseCatalogFixture(t)
	repository := NewInterBaseDBRepository(db)
	ctx := context.Background()

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
		table, name, typ, nullable, key, defaultValue string
		defaultValid                                  bool
	}{
		{table: "CHILD", name: "CHILD_B", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "CHILD", name: "CHILD_A", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "ID", typ: "INTEGER", nullable: "NO", key: "YES"},
		{table: "CUSTOMER", name: "CODE", typ: "CHAR(10)", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "CREATED", typ: "DATE", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "AMOUNT", typ: "NUMERIC(9, 2)", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "LABEL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'  seeded  '", defaultValid: true},
		{table: "CUSTOMER", name: "INHERITED", typ: "VARCHAR(20)", nullable: "NO", key: "NO", defaultValue: "'domain'", defaultValid: true},
		{table: "CUSTOMER", name: "OVERRIDE", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'column'", defaultValid: true},
		{table: "CUSTOMER", name: "DEFAULT_NULL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "NULL", defaultValid: true},
		{table: "CUSTOMER", name: "REQUIRED", typ: "INTEGER", nullable: "NO", key: "NO"},
		{table: "CUSTOMER", name: "DOUBLE_AMOUNT", typ: "DOUBLE PRECISION", nullable: "YES", key: "NO"},
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
			got.Null != want.nullable || got.Key != want.key {
			t.Errorf("column %d = (%q, %q, %q, %q, %q), want (%q, %q, %q, %q, %q)",
				i, got.Table, got.Name, got.Type, got.Null, got.Key,
				want.table, want.name, want.typ, want.nullable, want.key)
		}
		if got.Default.Valid != want.defaultValid || got.Default.String != want.defaultValue {
			t.Errorf("column %d default = %#v, want %#v", i, got.Default, sql.NullString{String: want.defaultValue, Valid: want.defaultValid})
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
			t.Errorf("schema column %d = %s.%s, want %s.%s", i, bySchema[i].Table, bySchema[i].Name, gotColumns[i].Table, gotColumns[i].Name)
		}
	}

	foreignKeys, err := repository.DescribeForeignKeysBySchema(ctx, "ignored-schema")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	if len(foreignKeys) != 1 || len(*foreignKeys[0]) != 2 {
		t.Fatalf("DescribeForeignKeysBySchema() = %#v, want one two-column foreign key", foreignKeys)
	}
	wantForeignKeyColumns := [][2]string{
		{"CHILD", "PARENT"},
		{"CHILD", "PARENT"},
	}
	wantForeignKeyNames := [][2]string{
		{"CHILD_B", "PARENT_B"},
		{"CHILD_A", "PARENT_A"},
	}
	for i, pair := range *foreignKeys[0] {
		if pair[0].Schema != "" || pair[1].Schema != "" ||
			pair[0].Table != wantForeignKeyColumns[i][0] || pair[1].Table != wantForeignKeyColumns[i][1] ||
			pair[0].Name != wantForeignKeyNames[i][0] || pair[1].Name != wantForeignKeyNames[i][1] {
			t.Errorf("foreign key pair %d = %#v, want %s.%s -> %s.%s", i, pair,
				wantForeignKeyColumns[i][0], wantForeignKeyNames[i][0],
				wantForeignKeyColumns[i][1], wantForeignKeyNames[i][1])
		}
	}
}

func TestInterBaseConfigBuildsLocalAndRemoteAttachmentsWithoutSecrets(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *DBConfig
		wantDSN     string
		wantCharset string
	}{
		{
			name: "explicit attachment and charset parameter",
			cfg: &DBConfig{
				Driver:         dialect.DatabaseDriverInterBase,
				DataSourceName: "server/3050:/srv/data/example.ib",
				User:           "alice",
				Passwd:         "do-not-log",
				Params:         map[string]string{"charset": "win1250"},
			},
			wantDSN:     "server/3050:/srv/data/example.ib",
			wantCharset: "WIN1250",
		},
		{
			name: "local path",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/var/lib/interbase/example.ib",
				User:   "alice",
			},
			wantDSN:     "/var/lib/interbase/example.ib",
			wantCharset: "UTF8",
		},
		{
			name: "remote defaults port",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Path:   "/var/lib/interbase/example.ib",
				User:   "alice",
			},
			wantDSN:     "db.example.test/3050:/var/lib/interbase/example.ib",
			wantCharset: "UTF8",
		},
		{
			name: "remote explicit port and db name fallback",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Port:   3307,
				DBName: "example.ib",
				User:   "alice",
			},
			wantDSN:     "db.example.test/3307:example.ib",
			wantCharset: "UTF8",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
			gotDSN, err := interBaseAttachment(test.cfg)
			if err != nil {
				t.Fatalf("interBaseAttachment() error = %v", err)
			}
			if gotDSN != test.wantDSN {
				t.Errorf("interBaseAttachment() = %q, want %q", gotDSN, test.wantDSN)
			}
			gotCharset, err := interBaseCharset(test.cfg)
			if err != nil {
				t.Fatalf("interBaseCharset() error = %v", err)
			}
			if gotCharset != test.wantCharset {
				t.Errorf("interBaseCharset() = %q, want %q", gotCharset, test.wantCharset)
			}
		})
	}
}

func TestInterBaseConfigRejectsUnsupportedConnectionModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  DBConfig
		want string
	}{
		{
			name: "missing user",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib"},
			want: "user",
		},
		{
			name: "missing attachment",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, User: "alice"},
			want: "dataSourceName",
		},
		{
			name: "unsupported protocol",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Proto: ProtoUDP, Path: "/tmp/example.ib", User: "alice"},
			want: "proto",
		},
		{
			name: "tcp requires host without explicit attachment",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Proto: ProtoTCP, Path: "/tmp/example.ib", User: "alice"},
			want: "host",
		},
		{
			name: "port range",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "db.example.test", Port: 65536, Path: "/tmp/example.ib", User: "alice"},
			want: "port",
		},
		{
			name: "explicit attachment still rejects unsupported protocol",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, DataSourceName: "example.ib", Proto: ProtoUDP, User: "alice"},
			want: "proto",
		},
		{
			name: "ssh is explicit unsupported",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib", User: "alice", SSHCfg: &SSHConfig{}},
			want: "SSH",
		},
		{
			name: "unsupported charset",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib", User: "alice", Passwd: "secret", Params: map[string]string{"charset": "latin1"}},
			want: "charset",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if err == nil {
				t.Fatal("DBConfig.Validate() returned nil error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("DBConfig.Validate() error = %q, want mention %q", err, test.want)
			}
			if strings.Contains(err.Error(), test.cfg.Passwd) && test.cfg.Passwd != "" {
				t.Fatalf("DBConfig.Validate() leaked password: %q", err)
			}
		})
	}
}

func TestInterBaseDriverIsRegistered(t *testing.T) {
	if !Registered(dialect.DatabaseDriverInterBase) {
		t.Fatalf("InterBase driver is not registered")
	}
	if _, err := CreateRepository(dialect.DatabaseDriverInterBase, nil); err != nil {
		t.Fatalf("CreateRepository() error = %v", err)
	}
}

func TestInterBaseOpenRejectsNilConfig(t *testing.T) {
	if _, err := Open(nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "config") {
		t.Fatalf("Open(nil) error = %v, want a configuration error", err)
	}
}

func TestInterBaseCatalogQueriesAvoidUnsupportedTrimFunction(t *testing.T) {
	queries := map[string]string{
		"relations":    interBaseRelationsQuery,
		"columns":      interBaseColumnsQuery,
		"foreign keys": interBaseForeignKeysQuery,
	}
	for name, query := range queries {
		if strings.Contains(strings.ToUpper(query), "TRIM(") {
			t.Errorf("%s catalog query contains unsupported TRIM function", name)
		}
	}
}

func openInterBaseCatalogFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(sqlite3) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	statements := []string{
		`CREATE TABLE "RDB$RELATIONS" ("RDB$RELATION_NAME" TEXT NOT NULL, "RDB$VIEW_BLR" TEXT, "RDB$SYSTEM_FLAG" INTEGER)`,
		`CREATE TABLE "RDB$RELATION_FIELDS" ("RDB$RELATION_NAME" TEXT NOT NULL, "RDB$FIELD_NAME" TEXT NOT NULL, "RDB$FIELD_SOURCE" TEXT NOT NULL, "RDB$FIELD_POSITION" INTEGER NOT NULL, "RDB$NULL_FLAG" INTEGER, "RDB$DEFAULT_SOURCE" TEXT)`,
		`CREATE TABLE "RDB$FIELDS" ("RDB$FIELD_NAME" TEXT NOT NULL, "RDB$FIELD_TYPE" INTEGER NOT NULL, "RDB$FIELD_SUB_TYPE" INTEGER, "RDB$FIELD_LENGTH" INTEGER, "RDB$FIELD_SCALE" INTEGER, "RDB$FIELD_PRECISION" INTEGER, "RDB$CHARACTER_LENGTH" INTEGER, "RDB$NULL_FLAG" INTEGER, "RDB$DEFAULT_SOURCE" TEXT)`,
		`CREATE TABLE "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT NOT NULL, "RDB$CONSTRAINT_TYPE" TEXT NOT NULL, "RDB$RELATION_NAME" TEXT NOT NULL, "RDB$INDEX_NAME" TEXT NOT NULL)`,
		`CREATE TABLE "RDB$REF_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT NOT NULL, "RDB$CONST_NAME_UQ" TEXT NOT NULL)`,
		`CREATE TABLE "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME" TEXT NOT NULL, "RDB$FIELD_NAME" TEXT NOT NULL, "RDB$FIELD_POSITION" INTEGER NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create catalog fixture table: %v", err)
		}
	}

	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("insert catalog fixture row: %v", err)
		}
	}
	insert(`INSERT INTO "RDB$RELATIONS" VALUES (?, ?, ?)`, interBaseFixed("CHILD"), nil, 0)
	insert(`INSERT INTO "RDB$RELATIONS" VALUES (?, ?, ?)`, interBaseFixed("CUSTOMER"), nil, 0)
	insert(`INSERT INTO "RDB$RELATIONS" VALUES (?, ?, ?)`, interBaseFixed("CUSTOMER_VIEW"), "view blr", 0)
	insert(`INSERT INTO "RDB$RELATIONS" VALUES (?, ?, ?)`, interBaseFixed("PARENT"), nil, 0)
	insert(`INSERT INTO "RDB$RELATIONS" VALUES (?, ?, ?)`, interBaseFixed("RDB$SYSTEM"), nil, 1)

	type field struct {
		relation, name, source string
		position               int
		nullFlag               any
		defaultSource          any
	}
	fields := []field{
		{relation: "CHILD", name: "CHILD_B", source: "CHILD_B", position: 0},
		{relation: "CHILD", name: "CHILD_A", source: "CHILD_A", position: 1},
		{relation: "CUSTOMER", name: "ID", source: "CUSTOMER_ID", position: 0, nullFlag: 1},
		{relation: "CUSTOMER", name: "CODE", source: "CUSTOMER_CODE", position: 1},
		{relation: "CUSTOMER", name: "CREATED", source: "CUSTOMER_CREATED", position: 2},
		{relation: "CUSTOMER", name: "AMOUNT", source: "CUSTOMER_AMOUNT", position: 3},
		{relation: "CUSTOMER", name: "LABEL", source: "CUSTOMER_LABEL", position: 4, defaultSource: " DEFAULT '  seeded  '   "},
		{relation: "CUSTOMER", name: "INHERITED", source: "CUSTOMER_INHERITED", position: 5},
		{relation: "CUSTOMER", name: "OVERRIDE", source: "CUSTOMER_OVERRIDE", position: 6, defaultSource: " DEFAULT 'column' "},
		{relation: "CUSTOMER", name: "DEFAULT_NULL", source: "CUSTOMER_DEFAULT_NULL", position: 7, defaultSource: " DEFAULT NULL "},
		{relation: "CUSTOMER", name: "REQUIRED", source: "CUSTOMER_REQUIRED", position: 8, nullFlag: 1},
		{relation: "CUSTOMER", name: "DOUBLE_AMOUNT", source: "CUSTOMER_DOUBLE_AMOUNT", position: 9},
		{relation: "CUSTOMER_VIEW", name: "VIEW_ID", source: "CUSTOMER_ID", position: 0},
		{relation: "PARENT", name: "PARENT_B", source: "PARENT_B", position: 0},
		{relation: "PARENT", name: "PARENT_A", source: "PARENT_A", position: 1},
	}
	for _, field := range fields {
		insert(`INSERT INTO "RDB$RELATION_FIELDS" VALUES (?, ?, ?, ?, ?, ?)`,
			interBaseFixed(field.relation), interBaseFixed(field.name), interBaseFixed(field.source),
			field.position, field.nullFlag, field.defaultSource)
	}

	type domain struct {
		name                         string
		fieldType, subtype, length   int
		scale, precision, charLength any
		nullFlag, defaultSource      any
	}
	domains := []domain{
		{name: "CHILD_B", fieldType: 8, subtype: 0, length: 4, scale: 0},
		{name: "CHILD_A", fieldType: 8, subtype: 0, length: 4, scale: 0},
		{name: "CUSTOMER_ID", fieldType: 8, subtype: 0, length: 4, scale: 0},
		{name: "CUSTOMER_CODE", fieldType: 14, subtype: 0, length: 10, scale: 0, charLength: 10},
		{name: "CUSTOMER_CREATED", fieldType: 35, subtype: 0, length: 8, scale: 0},
		{name: "CUSTOMER_AMOUNT", fieldType: 8, subtype: 1, length: 4, scale: -2, precision: 9},
		{name: "CUSTOMER_LABEL", fieldType: 37, subtype: 0, length: 20, scale: 0, charLength: 20},
		{name: "CUSTOMER_INHERITED", fieldType: 37, subtype: 0, length: 20, scale: 0, charLength: 20, nullFlag: 1, defaultSource: " DEFAULT 'domain' "},
		{name: "CUSTOMER_OVERRIDE", fieldType: 37, subtype: 0, length: 20, scale: 0, charLength: 20, defaultSource: " DEFAULT 'domain' "},
		{name: "CUSTOMER_DEFAULT_NULL", fieldType: 37, subtype: 0, length: 20, scale: 0, charLength: 20, defaultSource: " DEFAULT 'domain' "},
		{name: "CUSTOMER_REQUIRED", fieldType: 8, subtype: 0, length: 4, scale: 0},
		{name: "CUSTOMER_DOUBLE_AMOUNT", fieldType: 27, subtype: 1, length: 8, scale: 0, precision: 15},
		{name: "PARENT_B", fieldType: 8, subtype: 0, length: 4, scale: 0},
		{name: "PARENT_A", fieldType: 8, subtype: 0, length: 4, scale: 0},
	}
	for _, domain := range domains {
		insert(`INSERT INTO "RDB$FIELDS" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			interBaseFixed(domain.name), domain.fieldType, domain.subtype, domain.length,
			domain.scale, domain.precision, domain.charLength, domain.nullFlag, domain.defaultSource)
	}

	constraints := []struct{ name, typ, relation, index string }{
		{"PK_CUSTOMER", "PRIMARY KEY", "CUSTOMER", "IDX_CUSTOMER_PK"},
		{"PK_PARENT", "PRIMARY KEY", "PARENT", "IDX_PARENT_PK"},
		{"FK_CHILD", "FOREIGN KEY", "CHILD", "IDX_CHILD_FK"},
	}
	for _, constraint := range constraints {
		// SQLite TEXT does not implement InterBase CHAR padding semantics;
		// leave the type literal unpadded while keeping identifier columns
		// padded to exercise raw fixed-width joins.
		insert(`INSERT INTO "RDB$RELATION_CONSTRAINTS" VALUES (?, ?, ?, ?)`,
			interBaseFixed(constraint.name), constraint.typ,
			interBaseFixed(constraint.relation), interBaseFixed(constraint.index))
	}
	insert(`INSERT INTO "RDB$REF_CONSTRAINTS" VALUES (?, ?)`, interBaseFixed("FK_CHILD"), interBaseFixed("PK_PARENT"))
	segments := []struct {
		index, name string
		position    int
	}{
		{"IDX_CUSTOMER_PK", "ID", 0},
		{"IDX_PARENT_PK", "PARENT_B", 0},
		{"IDX_PARENT_PK", "PARENT_A", 1},
		{"IDX_CHILD_FK", "CHILD_B", 0},
		{"IDX_CHILD_FK", "CHILD_A", 1},
	}
	for _, segment := range segments {
		insert(`INSERT INTO "RDB$INDEX_SEGMENTS" VALUES (?, ?, ?)`,
			interBaseFixed(segment.index), interBaseFixed(segment.name), segment.position)
	}

	return db
}

func interBaseFixed(value string) string {
	return value + strings.Repeat(" ", 31-len(value))
}
