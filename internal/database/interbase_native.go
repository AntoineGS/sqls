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
func interBaseAttach(connCfg interBaseConnConfig, sqlDialect int) (*sql.DB, error) {
	return interBaseAttachContext(context.Background(), connCfg, sqlDialect)
}

// interBaseAttachContext opens and pings one pooled connection while honoring
// the caller's lifetime. The timeout bounds the ping without extending the
// caller's deadline.
func interBaseAttachContext(parent context.Context, connCfg interBaseConnConfig, sqlDialect int) (*sql.DB, error) {
	connector, err := interbase.NewConnector(interBaseDriverConfig(connCfg, sqlDialect))
	if err != nil {
		return nil, fmt.Errorf("interbase: create connector: %w", err)
	}

	conn := sql.OpenDB(connector)
	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	ctx, cancel := context.WithTimeout(parent, interBasePingTimeout)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		if parentErr := parent.Err(); parentErr != nil {
			return nil, parentErr
		}
		return nil, fmt.Errorf("interbase: ping failed: %w", err)
	}
	return conn, nil
}

// interBaseDriverConfig copies the untagged projection into the driver's
// configuration. Host and Database stay separate so the driver composes the
// attachment, including any TLS options.
func interBaseDriverConfig(cfg interBaseConnConfig, sqlDialect int) interbase.Config {
	return interbase.Config{
		Database:           cfg.Database,
		Host:               cfg.Host,
		User:               cfg.User,
		Password:           cfg.Password,
		Role:               cfg.Role,
		Charset:            cfg.Charset,
		CatalogTextCharset: cfg.CatalogTextCharset,
		Dialect:            sqlDialect,
		ConnectTimeout:     cfg.ConnectTimeout,
		TLS: interbase.TLSConfig{
			Enabled:              cfg.TLS.Enabled,
			ServerPublicFile:     cfg.TLS.ServerPublicFile,
			ServerPublicPath:     cfg.TLS.ServerPublicPath,
			ClientCertFile:       cfg.TLS.ClientCertFile,
			ClientPassPhrase:     cfg.TLS.ClientPassPhrase,
			ClientPassPhraseFile: cfg.TLS.ClientPassPhraseFile,
		},
	}
}

// interBaseDiagnostics reads the database's own answers over a pooled
// connection. Its error is never fatal: metadata introspection must not block
// editing, so the caller degrades to a default dialect and warns.
func interBaseDiagnostics(conn *sql.DB) (interbase.DatabaseDiagnostics, error) {
	return interBaseDiagnosticsContext(context.Background(), conn)
}

func interBaseDiagnosticsContext(parent context.Context, conn *sql.DB) (interbase.DatabaseDiagnostics, error) {
	ctx, cancel := context.WithTimeout(parent, interBasePingTimeout)
	defer cancel()

	pooled, err := conn.Conn(ctx)
	if err != nil {
		return interbase.DatabaseDiagnostics{}, err
	}
	defer func() { _ = pooled.Close() }()

	return interbase.Diagnostics(ctx, pooled)
}

func interBaseOpen(cfg *DBConfig) (*DBConnection, error) {
	return interBaseOpenContext(context.Background(), cfg)
}

func interBaseOpenContext(ctx context.Context, cfg *DBConfig) (*DBConnection, error) {
	return interBaseOpenContextWith(ctx, cfg, interBaseAttachContext, interBaseDiagnosticsContext)
}

func interBaseOpenContextWith(
	ctx context.Context,
	cfg *DBConfig,
	attach func(context.Context, interBaseConnConfig, int) (*sql.DB, error),
	diagnosticsFn func(context.Context, *sql.DB) (interbase.DatabaseDiagnostics, error),
) (*DBConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	connCfg, err := interBaseConnectionConfig(cfg)
	if err != nil {
		return nil, err
	}

	alias := cfg.Alias
	if alias == "" {
		alias = attachment
	}

	// The first attach uses the requested dialect; a requested zero means the
	// driver default, which is 3.
	conn, err := attach(ctx, connCfg, cfg.Dialect)
	if err != nil {
		return nil, err
	}

	diagnostics, diagErr := diagnosticsFn(ctx, conn)
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = conn.Close()
		return nil, ctxErr
	}
	decision := resolveInterBaseDialect(alias, cfg.Dialect, diagnostics.SQLDialect, diagErr)
	sourceDialect := decision.Resolved
	if diagErr == nil && (diagnostics.SQLDialect == 1 || diagnostics.SQLDialect == 3) {
		sourceDialect = int(diagnostics.SQLDialect)
	}

	if decision.Reattach {
		// The single extra attach, paid only by a Dialect 1 database.
		_ = conn.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err = attach(ctx, connCfg, decision.Resolved)
		if err != nil {
			return nil, err
		}
	}

	return &DBConnection{
		Conn:             conn,
		Driver:           dialect.DatabaseDriverInterBase,
		Variant:          dialect.InterBaseSQLVariant(decision.Resolved),
		SourceSQLDialect: sourceDialect,
		DatabaseName:     attachment,
		Warnings:         decision.Warnings,
	}, nil
}

var _ ExplainRepository = (*InterBaseDBRepository)(nil)
var _ InputDescriber = (*InterBaseDBRepository)(nil)

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

// DescribeInputs returns the server's metadata for a prepared statement's
// positional input parameters using one pooled connection.
func (db *InterBaseDBRepository) DescribeInputs(ctx context.Context, query string) ([]InputDescriptor, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	conn, err := db.Conn.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	driverInputs, err := interbase.DescribeInputs(ctx, conn, query)
	if err != nil {
		return nil, err
	}

	inputs := make([]InputDescriptor, len(driverInputs))
	for i, input := range driverInputs {
		inputs[i] = inputDescriptorFromDriver(input)
	}
	return inputs, nil
}

func inputDescriptorFromDriver(input interbase.InputDescriptor) InputDescriptor {
	return InputDescriptor{
		Kind:      input.Kind,
		Subtype:   input.Subtype,
		Scale:     input.Scale,
		Precision: input.Precision,
		Nullable:  input.Nullable,
	}
}
