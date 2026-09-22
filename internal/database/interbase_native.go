//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sqls-server/sqls/dialect"
	interbase "interbase-go"
)

const interBasePingTimeout = 10 * time.Second

// interBaseAttach opens and pings a pooled connection at one SQL dialect.
// A zero sqlDialect uses the driver default, which normalizeDialect maps to 3.
func interBaseAttach(cfg *DBConfig, attachment, charset string, sqlDialect int) (*sql.DB, error) {
	connector, err := interbase.NewConnector(interbase.Config{
		Database: attachment,
		User:     cfg.User,
		Password: cfg.Passwd,
		Charset:  charset,
		Dialect:  sqlDialect,
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: create connector: %w", err)
	}

	conn := sql.OpenDB(connector)
	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	ctx, cancel := context.WithTimeout(context.Background(), interBasePingTimeout)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("interbase: ping failed: %w", err)
	}
	return conn, nil
}

// interBaseDiagnostics reads the database's own answers over a pooled
// connection. Its error is never fatal: metadata introspection must not block
// editing, so the caller degrades to a default dialect and warns.
func interBaseDiagnostics(conn *sql.DB) (interbase.DatabaseDiagnostics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), interBasePingTimeout)
	defer cancel()

	pooled, err := conn.Conn(ctx)
	if err != nil {
		return interbase.DatabaseDiagnostics{}, err
	}
	defer func() { _ = pooled.Close() }()

	return interbase.Diagnostics(ctx, pooled)
}

func interBaseOpen(cfg *DBConfig) (*DBConnection, error) {
	if cfg == nil {
		return nil, errors.New("interbase: connection config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	attachment, err := interBaseAttachment(cfg)
	if err != nil {
		return nil, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return nil, err
	}

	alias := cfg.Alias
	if alias == "" {
		alias = attachment
	}

	// The first attach uses the requested dialect; a requested zero means the
	// driver default, which is 3.
	conn, err := interBaseAttach(cfg, attachment, charset, cfg.Dialect)
	if err != nil {
		return nil, err
	}

	diagnostics, diagErr := interBaseDiagnostics(conn)
	decision := resolveInterBaseDialect(alias, cfg.Dialect, diagnostics.SQLDialect, diagErr)

	if decision.Reattach {
		// The single extra attach, paid only by a Dialect 1 database.
		_ = conn.Close()
		conn, err = interBaseAttach(cfg, attachment, charset, decision.Resolved)
		if err != nil {
			return nil, err
		}
	}

	return &DBConnection{
		Conn:         conn,
		Driver:       dialect.DatabaseDriverInterBase,
		Variant:      dialect.InterBaseSQLVariant(decision.Resolved),
		DatabaseName: attachment,
		Warnings:     decision.Warnings,
	}, nil
}

var _ ExplainRepository = (*InterBaseDBRepository)(nil)

// ExplainPlan returns the server's query plan without executing the
// statement's result set. It acquires its own *sql.Conn because the plan is
// read from the prepared statement on that connection, and it returns the plan
// text unchanged.
//
// This is an interactive one-shot outside any cache build, so it uses the
// pooled *sql.DB rather than a catalog snapshot.
func (db *InterBaseDBRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	if db == nil || db.Conn == nil {
		return "", errors.New("interbase: database connection is nil")
	}
	conn, err := db.Conn.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	plan, err := interbase.Plan(ctx, conn, query)
	if err != nil {
		return "", fmt.Errorf("interbase: explain plan: %w", err)
	}
	return plan, nil
}
