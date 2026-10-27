package fbmeta

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
	ibmeta "github.com/sqls-server/sqls/metadata/interbase"
)

// Config is the Firebird attachment configuration used to identify a shared
// metadata store and its authenticated visibility.
type Config struct {
	Target         string
	User           string
	Password       string
	Role           string
	Dialect        int
	ConnectTimeout time.Duration
}

// KeyFor delegates to NormalizeTarget so every caller of this package shares
// exactly one attachment-normalization path.
func KeyFor(cfg Config) (metadata.Key, error) {
	return NormalizeTarget(cfg.Target)
}

// ScopeFor derives the authenticated visibility this Config observes. A blank
// principal cannot safely share permission-sensitive metadata coverage.
func ScopeFor(cfg Config) (metadata.Scope, error) {
	principal := strings.TrimSpace(cfg.User)
	if principal == "" {
		return metadata.Scope{}, metadata.ErrScopeRequired
	}
	return metadata.Scope{
		Principal:  principal,
		Role:       strings.TrimSpace(cfg.Role),
		TextPolicy: "NONE/WIN1252",
	}, nil
}

// Factory returns a metadata.Factory for cfg.
func Factory(cfg Config) metadata.Factory {
	return func(ctx context.Context) (metadata.Provider, error) {
		return newProvider(ctx, cfg)
	}
}

func newProvider(ctx context.Context, cfg Config) (metadata.Provider, error) {
	dsn, err := DriverDSN(cfg.Target, cfg.User, cfg.Password, cfg.Role, cfg.Dialect, url.Values{"none_text_fallback": {"WIN1252"}})
	if err != nil {
		return nil, err
	}
	conn, err := database.AttachFirebirdForMetadata(ctx, dsn, cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	dialect := cfg.Dialect
	if dialect != 1 {
		dialect = 3
	}
	repo := &database.InterBaseDBRepository{
		Conn: conn, SQLDialect: dialect, SourceSQLDialect: dialect,
		DatabaseName: stripScheme(cfg.Target), Firebird: true,
		FormatDDLHeader: ibmeta.FormatDDLHeader,
	}
	return &provider{conn: conn, repo: repo}, nil
}

type provider struct {
	conn *sql.DB
	repo *database.InterBaseDBRepository
}

func (p *provider) Plan() catalog.MetadataPlan { return p.repo.MetadataPlan() }

func (p *provider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return p.repo.MetadataDetailPlan(req)
}

func (p *provider) Close() error { return p.conn.Close() }
