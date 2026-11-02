// Package duckmeta adapts host-owned DuckDB connections to SQLs metadata.
// The host owns database opening and connection lifetime; this package never
// opens a DuckDB database itself.
package duckmeta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
)

// ConnSource supplies connections to the host-owned DuckDB instance.
type ConnSource interface {
	Conn(ctx context.Context) (*sql.Conn, error)
	Close() error
}

// Factory returns a metadata provider factory that obtains its connections
// from open and releases the source when the provider closes.
func Factory(cfg Config, open func(context.Context) (ConnSource, error)) metadata.Factory {
	return func(ctx context.Context) (metadata.Provider, error) {
		if ctx == nil {
			return nil, errors.New("duckmeta: metadata context is nil")
		}
		if _, err := ParseTarget(cfg.Target); err != nil {
			return nil, err
		}
		if open == nil {
			return nil, errors.New("duckmeta: connection source opener is nil")
		}
		source, err := open(ctx)
		if err != nil {
			return nil, fmt.Errorf("duckmeta: open metadata connection source: %w", err)
		}
		if source == nil {
			return nil, errors.New("duckmeta: connection source opener returned nil source")
		}

		conn, err := source.Conn(ctx)
		if err != nil {
			return nil, closeSourceWithError(source, fmt.Errorf("duckmeta: acquire catalog connection: %w", err))
		}
		if conn == nil {
			return nil, closeSourceWithError(source, errors.New("duckmeta: connection source returned nil connection"))
		}
		var catalogName string
		queryErr := conn.QueryRowContext(ctx, "SELECT current_database()").Scan(&catalogName)
		connCloseErr := conn.Close()
		if queryErr != nil || connCloseErr != nil {
			err = errors.Join(queryErr, wrapDuckDBCloseError("close catalog connection", connCloseErr))
			return nil, closeSourceWithError(source, fmt.Errorf("duckmeta: read current catalog: %w", err))
		}
		if catalogName == "" {
			return nil, closeSourceWithError(source, errors.New("duckmeta: DuckDB returned an empty current catalog"))
		}

		repo := &database.DuckDBMetadataRepository{Conn: source.Conn, Catalog: catalogName}
		return &duckDBProvider{source: source, repo: repo}, nil
	}
}

type duckDBProvider struct {
	source ConnSource
	repo   *database.DuckDBMetadataRepository
	once   sync.Once
	err    error
}

func (p *duckDBProvider) Plan() catalog.MetadataPlan {
	if p == nil || p.repo == nil {
		return catalog.MetadataPlan{}
	}
	return p.repo.MetadataPlan()
}

func (p *duckDBProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if p == nil || p.repo == nil {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return p.repo.MetadataDetailPlan(req)
}

func (p *duckDBProvider) Close() error {
	if p == nil || p.source == nil {
		return nil
	}
	p.once.Do(func() { p.err = p.source.Close() })
	return p.err
}

func closeSourceWithError(source ConnSource, err error) error {
	if source == nil {
		return err
	}
	if closeErr := source.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("duckmeta: close metadata connection source: %w", closeErr))
	}
	return err
}

func wrapDuckDBCloseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("duckmeta: %s: %w", operation, err)
}

var _ metadata.Provider = (*duckDBProvider)(nil)
