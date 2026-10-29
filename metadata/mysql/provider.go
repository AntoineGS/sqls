package mymeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
)

// Factory returns a metadata-store provider factory for cfg. Credentials stay
// inside this closure and the driver DSN; neither enters metadata identity.
func Factory(cfg Config) metadata.Factory {
	return func(ctx context.Context) (metadata.Provider, error) {
		return newProvider(ctx, cfg)
	}
}

type mysqlProvider struct {
	conn *sql.DB
	repo *database.MySQLMetadataRepository
}

func newProvider(ctx context.Context, cfg Config) (metadata.Provider, error) {
	return newProviderWithOpen(ctx, cfg, sql.Open)
}

func newProviderWithOpen(ctx context.Context, cfg Config, open func(string, string) (*sql.DB, error)) (metadata.Provider, error) {
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		return nil, err
	}
	if open == nil {
		return nil, errors.New("mymeta: MySQL database opener is nil")
	}
	dsn, err := DriverDSN(cfg, nil)
	if err != nil {
		return nil, err
	}
	conn, err := open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mymeta: open metadata database: %w", err)
	}
	if conn == nil {
		return nil, errors.New("mymeta: database opener returned nil database")
	}
	conn.SetMaxOpenConns(3)
	if err := conn.PingContext(ctx); err != nil {
		closeErr := conn.Close()
		pingErr := fmt.Errorf("mymeta: ping metadata database: %w", err)
		if closeErr != nil {
			return nil, errors.Join(pingErr, fmt.Errorf("mymeta: close failed metadata database: %w", closeErr))
		}
		return nil, pingErr
	}
	repo := &database.MySQLMetadataRepository{Conn: conn, Schema: target.Schema}
	return &mysqlProvider{conn: conn, repo: repo}, nil
}

func (p *mysqlProvider) Plan() catalog.MetadataPlan {
	return p.repo.MetadataPlan()
}

func (p *mysqlProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return p.repo.MetadataDetailPlan(req)
}

func (p *mysqlProvider) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}
