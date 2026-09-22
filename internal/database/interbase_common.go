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

	// snapshot is set only on a repository returned by CatalogSnapshot. It
	// binds this repository to one read-only transaction that has already read
	// relations and constraints. The source repository is never mutated, so a
	// cache build on the worker goroutine cannot race a ReCache on a handler
	// goroutine.
	snapshot *interBaseCatalogSnapshot
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
