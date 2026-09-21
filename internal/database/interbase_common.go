package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/dialect"
)

const interBaseDefaultPort = 3050

func init() {
	RegisterOpen(dialect.DatabaseDriverInterBase, interBaseOpen)
	RegisterFactory(dialect.DatabaseDriverInterBase, NewInterBaseDBRepository)
	RegisterConnFactory(dialect.DatabaseDriverInterBase, NewInterBaseDBRepositoryFromConnection)
}

// interBaseAttachment converts the existing DBConfig fields to the native
// InterBase attachment format. DataSourceName is already an attachment
// string; otherwise Path (or DBName for compatibility with existing configs)
// is used as the database path.
func interBaseAttachment(cfg *DBConfig) (string, error) {
	if cfg == nil {
		return "", errors.New("interbase: connection config is nil")
	}
	if cfg.Proto != "" && cfg.Proto != ProtoTCP {
		return "", fmt.Errorf("interbase: unsupported protocol %q", cfg.Proto)
	}
	if cfg.Port < 0 {
		return "", errors.New("interbase: port cannot be negative")
	}
	if cfg.Port > 65535 {
		return "", errors.New("interbase: port cannot exceed 65535")
	}
	if cfg.DataSourceName != "" {
		return cfg.DataSourceName, nil
	}
	if cfg.Proto == ProtoTCP && cfg.Host == "" {
		return "", errors.New("interbase: required host for tcp protocol")
	}

	databasePath := cfg.Path
	if databasePath == "" {
		databasePath = cfg.DBName
	}
	if databasePath == "" {
		return "", errors.New("interbase: required dataSourceName, path, or dbName")
	}
	if cfg.Host == "" {
		if cfg.Port != 0 {
			return "", errors.New("interbase: port requires a host")
		}
		return databasePath, nil
	}

	port := cfg.Port
	if port == 0 {
		port = interBaseDefaultPort
	}
	return fmt.Sprintf("%s/%d:%s", cfg.Host, port, databasePath), nil
}

func interBaseCharset(cfg *DBConfig) (string, error) {
	if cfg == nil {
		return "", errors.New("interbase: connection config is nil")
	}

	charset := ""
	found := false
	for key, value := range cfg.Params {
		if !strings.EqualFold(strings.TrimSpace(key), "charset") {
			continue
		}
		if found && !strings.EqualFold(strings.TrimSpace(charset), strings.TrimSpace(value)) {
			return "", errors.New("interbase: conflicting charset parameters")
		}
		charset = value
		found = true
	}

	switch strings.ToUpper(strings.TrimSpace(charset)) {
	case "":
		return "UTF8", nil
	case "UTF8", "WIN1250":
		return strings.ToUpper(strings.TrimSpace(charset)), nil
	default:
		return "", fmt.Errorf("interbase: unsupported charset %q", charset)
	}
}

type InterBaseDBRepository struct {
	Conn *sql.DB
	// SQLDialect is 1 or 3; zero is treated as 3, matching the driver default.
	SQLDialect int
	// DatabaseName is the attachment string; empty when unknown.
	DatabaseName string
}

var _ DBRepository = (*InterBaseDBRepository)(nil)

// NewInterBaseDBRepository builds a repository from a pooled *sql.DB alone.
// It has no connection context, so it leaves SQLDialect zero (dialect 3) and
// DatabaseName empty.
func NewInterBaseDBRepository(conn *sql.DB) DBRepository {
	return &InterBaseDBRepository{Conn: conn}
}

// NewInterBaseDBRepositoryFromConnection builds a repository that knows the
// SQL dialect resolved at connect and the attachment it was resolved for.
func NewInterBaseDBRepositoryFromConnection(conn *DBConnection) DBRepository {
	if conn == nil {
		return &InterBaseDBRepository{}
	}
	return &InterBaseDBRepository{
		Conn:         conn.Conn,
		SQLDialect:   conn.Variant.InterBaseSQLDialect(),
		DatabaseName: conn.DatabaseName,
	}
}

func (db *InterBaseDBRepository) Driver() dialect.DatabaseDriver {
	return dialect.DatabaseDriverInterBase
}

// InterBase has one database per attachment and has no database catalog that
// can be enumerated through this repository abstraction.
func (db *InterBaseDBRepository) CurrentDatabase(context.Context) (string, error) {
	return "", nil
}

func (db *InterBaseDBRepository) Databases(context.Context) ([]string, error) {
	return []string{}, nil
}

// InterBase does not have a schema namespace in the same sense as the other
// supported servers. The empty schema keeps the shared cache and completion
// paths usable without inventing a server-side name.
func (db *InterBaseDBRepository) CurrentSchema(context.Context) (string, error) {
	return "", nil
}

func (db *InterBaseDBRepository) Schemas(context.Context) ([]string, error) {
	return []string{""}, nil
}

func (db *InterBaseDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	tables, err := db.relationNames(ctx, interBaseRelationsQuery)
	if err != nil {
		return nil, err
	}
	return map[string][]string{"": tables}, nil
}

func (db *InterBaseDBRepository) relationNames(ctx context.Context, query string) ([]string, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	rows, err := db.Conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name.Valid {
			result = append(result, strings.TrimSpace(name.String))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

const interBaseRelationsQuery = `
SELECT RDB$RELATION_NAME
  FROM RDB$RELATIONS
 WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0
 ORDER BY RDB$RELATION_NAME
`

const interBaseColumnsQuery = `
SELECT
    rf.RDB$RELATION_NAME,
    rf.RDB$FIELD_NAME,
    rf.RDB$NULL_FLAG,
    rf.RDB$DEFAULT_SOURCE,
    f.RDB$NULL_FLAG,
    f.RDB$DEFAULT_SOURCE,
    f.RDB$FIELD_TYPE,
    f.RDB$FIELD_SUB_TYPE,
    f.RDB$FIELD_LENGTH,
    f.RDB$FIELD_SCALE,
    f.RDB$FIELD_PRECISION,
    f.RDB$CHARACTER_LENGTH,
    CASE WHEN EXISTS (
        SELECT 1
          FROM RDB$RELATION_CONSTRAINTS pc
          JOIN RDB$INDEX_SEGMENTS ps
            ON ps.RDB$INDEX_NAME = pc.RDB$INDEX_NAME
         WHERE pc.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'
           AND pc.RDB$RELATION_NAME = rf.RDB$RELATION_NAME
           AND ps.RDB$FIELD_NAME = rf.RDB$FIELD_NAME
    ) THEN 'YES' ELSE 'NO' END
  FROM RDB$RELATION_FIELDS rf
  JOIN RDB$RELATIONS r
    ON r.RDB$RELATION_NAME = rf.RDB$RELATION_NAME
  JOIN RDB$FIELDS f
    ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
 WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
 ORDER BY rf.RDB$RELATION_NAME, rf.RDB$FIELD_POSITION
`

type interBaseColumnRow struct {
	relationName    sql.NullString
	fieldName       sql.NullString
	nullFlag        sql.NullInt64
	defaultSource   sql.NullString
	domainNullFlag  sql.NullInt64
	domainDefault   sql.NullString
	fieldType       sql.NullInt64
	fieldSubtype    sql.NullInt64
	fieldLength     sql.NullInt64
	fieldScale      sql.NullInt64
	fieldPrecision  sql.NullInt64
	characterLength sql.NullInt64
	primaryKey      sql.NullString
}

func (db *InterBaseDBRepository) DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) DescribeDatabaseTableBySchema(ctx context.Context, _ string) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) describeColumns(ctx context.Context) ([]*ColumnDesc, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	rows, err := db.Conn.QueryContext(ctx, interBaseColumnsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]*ColumnDesc, 0)
	for rows.Next() {
		var row interBaseColumnRow
		if err := rows.Scan(
			&row.relationName,
			&row.fieldName,
			&row.nullFlag,
			&row.defaultSource,
			&row.domainNullFlag,
			&row.domainDefault,
			&row.fieldType,
			&row.fieldSubtype,
			&row.fieldLength,
			&row.fieldScale,
			&row.fieldPrecision,
			&row.characterLength,
			&row.primaryKey,
		); err != nil {
			return nil, err
		}
		column, err := interBaseColumnDescription(row)
		if err != nil {
			return nil, err
		}
		result = append(result, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func interBaseColumnDescription(row interBaseColumnRow) (*ColumnDesc, error) {
	if !row.relationName.Valid || !row.fieldName.Valid {
		return nil, errors.New("interbase: catalog returned a column without a name")
	}
	if !row.fieldType.Valid {
		return nil, errors.New("interbase: catalog returned a column without a field type")
	}
	typ := interBaseColumnType(row)
	key := strings.TrimSpace(row.primaryKey.String)
	if key != "YES" {
		key = "NO"
	}
	return &ColumnDesc{
		ColumnBase: ColumnBase{
			Schema: "",
			Table:  strings.TrimSpace(row.relationName.String),
			Name:   strings.TrimSpace(row.fieldName.String),
		},
		Type:    typ,
		Null:    interBaseNullability(row.nullFlag, row.domainNullFlag),
		Key:     key,
		Default: interBaseEffectiveDefault(row.defaultSource, row.domainDefault),
		Extra:   "",
	}, nil
}

func interBaseNullability(columnNullFlag, domainNullFlag sql.NullInt64) string {
	if (columnNullFlag.Valid && columnNullFlag.Int64 != 0) ||
		(domainNullFlag.Valid && domainNullFlag.Int64 != 0) {
		return "NO"
	}
	return "YES"
}

func interBaseDefault(source sql.NullString) sql.NullString {
	if !source.Valid {
		return sql.NullString{}
	}
	value := strings.TrimSpace(source.String)
	if value == "" {
		return sql.NullString{}
	}
	if len(value) >= len("DEFAULT") && strings.EqualFold(value[:len("DEFAULT")], "DEFAULT") {
		if len(value) == len("DEFAULT") || value[len("DEFAULT")] == ' ' || value[len("DEFAULT")] == '\t' || value[len("DEFAULT")] == '\n' || value[len("DEFAULT")] == '\r' {
			value = strings.TrimSpace(value[len("DEFAULT"):])
		}
	}
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func interBaseEffectiveDefault(columnSource, domainSource sql.NullString) sql.NullString {
	if columnSource.Valid {
		return interBaseDefault(columnSource)
	}
	return interBaseDefault(domainSource)
}

func interBaseColumnType(row interBaseColumnRow) string {
	fieldType := row.fieldType.Int64
	switch fieldType {
	case 7:
		return interBaseNumericType("SMALLINT", 4, row)
	case 8:
		return interBaseNumericType("INTEGER", 9, row)
	case 9:
		return "QUAD"
	case 10:
		return "FLOAT"
	case 12:
		return "DATE"
	case 13:
		return "TIME"
	case 14:
		return fmt.Sprintf("CHAR(%d)", interBaseCharacterLength(row))
	case 16:
		return interBaseNumericType("BIGINT", 18, row)
	case 17:
		return "BOOLEAN"
	case 27:
		// In Dialect 1, scaled NUMERIC/DECIMAL values can use DOUBLE
		// PRECISION as their underlying field type. A subtype without a
		// negative scale does not carry a fixed-point declaration, so keep
		// the underlying DOUBLE PRECISION rather than inventing (15, 0).
		if row.fieldScale.Valid && row.fieldScale.Int64 < 0 {
			return interBaseNumericType("DOUBLE PRECISION", 15, row)
		}
		return "DOUBLE PRECISION"
	case 35:
		// Dialect 1 uses field type 35 for DATE. Dialect 3's timestamp
		// distinction is deliberately not inferred by this fixed-dialect
		// adapter.
		return "DATE"
	case 37:
		return fmt.Sprintf("VARCHAR(%d)", interBaseCharacterLength(row))
	case 40:
		return fmt.Sprintf("CSTRING(%d)", interBaseCharacterLength(row))
	case 45:
		return "BLOB_ID"
	case 261:
		return "BLOB"
	default:
		return fmt.Sprintf("TYPE(%d)", fieldType)
	}
}

func interBaseCharacterLength(row interBaseColumnRow) int64 {
	if row.characterLength.Valid {
		return row.characterLength.Int64
	}
	if row.fieldLength.Valid {
		return row.fieldLength.Int64
	}
	return 0
}

func interBaseNumericType(base string, naturalPrecision int64, row interBaseColumnRow) string {
	subtype := int64(0)
	if row.fieldSubtype.Valid {
		subtype = row.fieldSubtype.Int64
	}
	scale := int64(0)
	if row.fieldScale.Valid {
		scale = row.fieldScale.Int64
	}
	if subtype == 0 && scale >= 0 {
		return base
	}

	precision := naturalPrecision
	if row.fieldPrecision.Valid && row.fieldPrecision.Int64 > 0 {
		precision = row.fieldPrecision.Int64
	}
	numericName := "NUMERIC"
	if subtype == 2 {
		numericName = "DECIMAL"
	}
	if scale < 0 {
		scale = -scale
	}
	return fmt.Sprintf("%s(%d, %d)", numericName, precision, scale)
}

const interBaseForeignKeysQuery = `
SELECT
    fk.RDB$CONSTRAINT_NAME,
    fk.RDB$RELATION_NAME,
    fkseg.RDB$FIELD_NAME,
    uq.RDB$RELATION_NAME,
    uqseg.RDB$FIELD_NAME
  FROM RDB$RELATION_CONSTRAINTS fk
  JOIN RDB$REF_CONSTRAINTS ref
    ON ref.RDB$CONSTRAINT_NAME = fk.RDB$CONSTRAINT_NAME
  JOIN RDB$RELATION_CONSTRAINTS uq
    ON uq.RDB$CONSTRAINT_NAME = ref.RDB$CONST_NAME_UQ
  JOIN RDB$RELATIONS fkrel
    ON fkrel.RDB$RELATION_NAME = fk.RDB$RELATION_NAME
  JOIN RDB$RELATIONS uqrel
    ON uqrel.RDB$RELATION_NAME = uq.RDB$RELATION_NAME
  JOIN RDB$INDEX_SEGMENTS fkseg
    ON fkseg.RDB$INDEX_NAME = fk.RDB$INDEX_NAME
  JOIN RDB$INDEX_SEGMENTS uqseg
    ON uqseg.RDB$INDEX_NAME = uq.RDB$INDEX_NAME
   AND uqseg.RDB$FIELD_POSITION = fkseg.RDB$FIELD_POSITION
 WHERE fk.RDB$CONSTRAINT_TYPE = 'FOREIGN KEY'
   AND COALESCE(fkrel.RDB$SYSTEM_FLAG, 0) = 0
   AND COALESCE(uqrel.RDB$SYSTEM_FLAG, 0) = 0
 ORDER BY fk.RDB$CONSTRAINT_NAME, fkseg.RDB$FIELD_POSITION
`

func (db *InterBaseDBRepository) DescribeForeignKeysBySchema(ctx context.Context, _ string) ([]*ForeignKey, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	rows, err := db.Conn.QueryContext(ctx, interBaseForeignKeysQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return parseInterBaseForeignKeys(rows)
}

func parseInterBaseForeignKeys(rows *sql.Rows) ([]*ForeignKey, error) {
	foreignKeys := make([]*ForeignKey, 0)
	var currentID string
	var current *ForeignKey

	for rows.Next() {
		var rawID, rawTable, rawColumn, rawRefTable, rawRefColumn sql.NullString
		if err := rows.Scan(&rawID, &rawTable, &rawColumn, &rawRefTable, &rawRefColumn); err != nil {
			return nil, err
		}
		if !rawID.Valid || !rawTable.Valid || !rawColumn.Valid || !rawRefTable.Valid || !rawRefColumn.Valid {
			return nil, errors.New("interbase: catalog returned an incomplete foreign key")
		}

		fkID := strings.TrimSpace(rawID.String)
		if current == nil || fkID != currentID {
			if current != nil {
				foreignKeys = append(foreignKeys, current)
			}
			current = new(ForeignKey)
			currentID = fkID
		}
		left := &ColumnBase{
			Schema: "",
			Table:  strings.TrimSpace(rawTable.String),
			Name:   strings.TrimSpace(rawColumn.String),
		}
		right := &ColumnBase{
			Schema: "",
			Table:  strings.TrimSpace(rawRefTable.String),
			Name:   strings.TrimSpace(rawRefColumn.String),
		}
		*current = append(*current, [2]*ColumnBase{left, right})
	}
	if current != nil {
		foreignKeys = append(foreignKeys, current)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return foreignKeys, nil
}

func (db *InterBaseDBRepository) Exec(ctx context.Context, query string) (sql.Result, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	return db.Conn.ExecContext(ctx, query)
}

func (db *InterBaseDBRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	return db.Conn.QueryContext(ctx, query)
}

// QueryReadOnly runs a read statement inside an explicit read-only,
// read-committed transaction and materialises the whole result before
// returning, so the transaction's lifetime never escapes this method and an
// early return in the handler cannot leak it.
//
// EXECUTE PROCEDURE deliberately never reaches this path: per the driver's
// documented boundary an implicit procedure query commits its write
// transaction, so a procedure call is a write even when it returns a row.
func (db *InterBaseDBRepository) QueryReadOnly(ctx context.Context, query string) (*QueryResult, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}

	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		return nil, err
	}
	// A read-only transaction is released by rolling it back; there is nothing
	// to commit, and the rollback must run on every path including a partial
	// fetch.
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return ScanRowsWithTypes(rows, RenderOptionsFor(dialect.DatabaseDriverInterBase))
}

var _ ReadOnlyQuerier = (*InterBaseDBRepository)(nil)
