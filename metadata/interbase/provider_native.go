//go:build interbase && cgo && linux && amd64

package ibmeta

import (
	"context"
	"database/sql"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
)

// nativeProvider is the real, build-tagged Provider: a store-owned InterBase
// attachment (independent of any query/execution connection a caller might
// separately hold) plus the same InterBaseDBRepository bulk-category and
// detail adaptation exercised in internal/database. It never falls back to
// an unconditional schema.New(db.Conn) refetch: Plan/DetailPlan delegate
// straight to the repository's coordinated MetadataPlan/MetadataDetailPlan.
type nativeProvider struct {
	conn *sql.DB
	repo *database.InterBaseDBRepository
}

func newProvider(ctx context.Context, cfg Config) (metadata.Provider, error) {
	conn, err := database.AttachInterBaseForMetadata(ctx, database.InterBaseMetadataAttachConfig{
		Database:                target(cfg.Target),
		User:                    cfg.User,
		Password:                cfg.Password,
		Role:                    cfg.Role,
		Charset:                 cfg.Charset,
		CatalogTextCharset:      cfg.CatalogTextCharset,
		ConnectTimeout:          cfg.ConnectTimeout,
		TLSEnabled:              cfg.TLS.Enabled,
		TLSServerPublicFile:     cfg.TLS.ServerPublicFile,
		TLSServerPublicPath:     cfg.TLS.ServerPublicPath,
		TLSClientCertFile:       cfg.TLS.ClientCertFile,
		TLSClientPassPhrase:     cfg.TLS.ClientPassPhrase,
		TLSClientPassPhraseFile: cfg.TLS.ClientPassPhraseFile,
	}, cfg.Dialect)
	if err != nil {
		return nil, err
	}
	repo := &database.InterBaseDBRepository{
		Conn:             conn,
		SQLDialect:       cfg.Dialect,
		SourceSQLDialect: cfg.Dialect,
		DatabaseName:     cfg.Target,
	}
	return &nativeProvider{conn: conn, repo: repo}, nil
}

func (p *nativeProvider) Plan() catalog.MetadataPlan { return p.repo.MetadataPlan() }

func (p *nativeProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return p.repo.MetadataDetailPlan(req)
}

func (p *nativeProvider) Close() error { return p.conn.Close() }

// target strips a leading "ib://" scheme, which NormalizeTarget/KeyFor
// canonicalize but the driver's own attachment string never carries.
func target(raw string) string {
	const scheme = "ib://"
	if len(raw) >= len(scheme) && equalFoldASCII(raw[:len(scheme)], scheme) {
		return raw[len(scheme):]
	}
	return raw
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
