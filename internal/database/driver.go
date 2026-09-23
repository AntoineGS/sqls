package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/sqls-server/sqls/dialect"
	"golang.org/x/crypto/ssh"
)

var driverOpeners = make(map[dialect.DatabaseDriver]Opener)
var driverContextOpeners = make(map[dialect.DatabaseDriver]ContextOpener)
var driverFactories = make(map[dialect.DatabaseDriver]Factory)
var driverConnFactories = make(map[dialect.DatabaseDriver]ConnFactory)

type Opener func(*DBConfig) (*DBConnection, error)
type ContextOpener func(context.Context, *DBConfig) (*DBConnection, error)
type Factory func(*sql.DB) DBRepository

// ConnFactory builds a repository from the whole connection rather than from
// the *sql.DB alone, for drivers whose repository needs connection-level
// context such as a resolved SQL variant. It is optional: a driver that
// registers none keeps using Factory.
type ConnFactory func(*DBConnection) DBRepository

type DBConnection struct {
	Conn    *sql.DB
	SSHConn *ssh.Client
	Tunnel  io.Closer
	Driver  dialect.DatabaseDriver

	// Variant is the server-side SQL variant resolved at connect. It is empty
	// for drivers that have no variants.
	Variant dialect.SQLVariant
	// SourceSQLDialect is the dialect reported by the attached InterBase
	// database. It is independent of Variant, which is the client attachment
	// dialect selected for lexing and queries.
	SourceSQLDialect int
	// DatabaseName identifies the attached database for drivers with a single
	// attachment per connection. Empty when the driver enumerates databases.
	DatabaseName string
	// Warnings are non-fatal connect-time diagnostics for the user.
	Warnings []string
}

// DriverVariant pairs the driver with the resolved variant. Nil-safe.
func (db *DBConnection) DriverVariant() dialect.DriverVariant {
	if db == nil {
		return dialect.DriverVariant{}
	}
	return dialect.DriverVariant{Driver: db.Driver, Variant: db.Variant}
}

func (db *DBConnection) Close() error {
	if db == nil {
		return nil
	}
	var closeErrors []error
	if db.Conn != nil {
		if err := db.Conn.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if db.SSHConn != nil {
		if err := db.SSHConn.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if db.Tunnel != nil {
		if err := db.Tunnel.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func RegisterOpen(name dialect.DatabaseDriver, opener Opener) {
	if _, ok := driverOpeners[name]; ok {
		panic(fmt.Sprintf("driver open %s method is already registered", name))
	}
	driverOpeners[name] = opener
}

// RegisterOpenContext registers an optional context-aware opener. It is a
// separate capability so existing DBRepository implementations and legacy
// drivers do not need to change.
func RegisterOpenContext(name dialect.DatabaseDriver, opener ContextOpener) {
	if _, ok := driverContextOpeners[name]; ok {
		panic(fmt.Sprintf("driver context open %s method is already registered", name))
	}
	driverContextOpeners[name] = opener
}

func RegisterFactory(name dialect.DatabaseDriver, factory Factory) {
	if _, ok := driverFactories[name]; ok {
		panic(fmt.Sprintf("driver factory %s already registered", name))
	}
	driverFactories[name] = factory
}

func Registered(name dialect.DatabaseDriver) bool {
	_, ok1 := driverOpeners[name]
	_, ok2 := driverFactories[name]
	return ok1 && ok2
}

func Open(cfg *DBConfig) (*DBConnection, error) {
	if cfg == nil {
		return nil, fmt.Errorf("connection config is nil")
	}
	OpenFn, ok := driverOpeners[cfg.Driver]
	if !ok {
		return nil, fmt.Errorf("driver not found, %s", cfg.Driver)
	}
	return OpenFn(cfg)
}

// OpenContext opens a connection, preferring a context-aware driver opener.
// Legacy openers are called synchronously: an uncancellable open must never be
// left running in a detached goroutine. If cancellation wins while an opener
// is returning a connection, that candidate is closed instead of being handed
// to the caller.
func OpenContext(ctx context.Context, cfg *DBConfig) (*DBConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, fmt.Errorf("connection config is nil")
	}

	if opener, ok := driverContextOpeners[cfg.Driver]; ok {
		conn, err := opener(ctx, cfg)
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = conn.Close()
			return nil, ctxErr
		}
		return conn, err
	}

	conn, err := Open(cfg)
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = conn.Close()
		return nil, ctxErr
	}
	return conn, err
}

func CreateRepository(driver dialect.DatabaseDriver, db *sql.DB) (DBRepository, error) {
	FactoryFn, ok := driverFactories[driver]
	if !ok {
		return nil, fmt.Errorf("driver not found, %s", driver)
	}
	return FactoryFn(db), nil
}

func RegisterConnFactory(name dialect.DatabaseDriver, factory ConnFactory) {
	if _, ok := driverConnFactories[name]; ok {
		panic(fmt.Sprintf("driver conn factory %s already registered", name))
	}
	driverConnFactories[name] = factory
}

// CreateRepositoryFromConnection builds a repository for a connection,
// preferring a registered ConnFactory and falling back to the *sql.DB factory
// so that drivers which register no ConnFactory are unaffected.
//
// The driver is passed in rather than read from conn.Driver because that field
// is not populated by every opener: openPostgreSQL (postgresql.go:55),
// openSQLite3 (sqlite3.go:24) and the "mock" opener (database_mock.go:549) all
// return a DBConnection with an empty Driver. The caller's configured driver is
// always populated, so keying the lookup off conn.Driver would return
// "driver not found" for PostgreSQL, SQLite3 and every handler test.
func CreateRepositoryFromConnection(driver dialect.DatabaseDriver, conn *DBConnection) (DBRepository, error) {
	if conn == nil {
		return nil, fmt.Errorf("connection is nil")
	}
	if factory, ok := driverConnFactories[driver]; ok {
		return factory(conn), nil
	}
	return CreateRepository(driver, conn.Conn)
}
