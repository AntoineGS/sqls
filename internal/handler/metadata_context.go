package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
	ibmeta "github.com/sqls-server/sqls/metadata/interbase"
)

// metadataTargetFor derives the shared-metadata Key, Scope and Factory for
// cfg. InterBase reuses A5's real ibmeta.Factory (a store-owned attachment,
// independent of the host's own query connection, with coordinated bulk and
// on-demand detail loading). Every other driver uses a minimal generic
// Factory (Ruling R8): its Provider builds a repository via the existing
// database.CreateRepositoryFromConnection/MetadataPlanFor pair — the same
// repo/queries as today, no second connection pool, no on-demand detail
// (DetailPlan returns metadata.ErrUnsupported) — from liveConn, resolved
// dynamically at Plan() time rather than captured once when the Factory
// runs. This matters because Lease.Refresh (an explicit same-target
// reselect) re-runs Plan() on the SAME Provider instance a connection swap
// later: attachIntent always opens a new connection and enqueues the old one
// for closing before bindMetadata ever runs, so a Provider that captured a
// static connection at Factory-creation time would run its catalog query
// against a connection that is being closed by the cleanup worker. Reading
// liveConn() at Plan() time instead always observes whichever connection is
// actually live at that moment.
//
// Key/Scope design for the generic path (no prior task specifies this):
//   - Key.URL is a driver-neutral, structured projection of cfg's connection
//     identity (proto/host/port/path/dbName), matching the same fields
//     intentKey already uses to distinguish one target from another, but never
//     the password. When cfg has no structured host (a raw DataSourceName,
//     e.g. many non-InterBase DSNs), Key.URL falls back to the DSN verbatim:
//     some drivers' DSN strings embed credentials in that case, the same
//     exposure intentKey/activeConfigKey already carry internally today (see
//     doc/metadata-sharing.md); this is a known, disclosed limitation of the
//     generic path, not a new one.
//   - Scope.Principal is cfg.User when set, else a constant non-empty
//     placeholder ("sqls-generic-unauthenticated"): most generic drivers this
//     repository supports today (SQLite, and any driver reached through a
//     bare DataSourceName) have no meaningful separate-principal concept, and
//     Registry.Acquire rejects a zero Scope outright (ErrScopeRequired), so a
//     constant placeholder is the only value that keeps normal generic
//     connections sharing one store without inventing a permission model this
//     increment does not have data for.
func metadataTargetFor(cfg *database.DBConfig, liveConn func() *database.DBConnection) (metadata.Key, metadata.Scope, metadata.Factory, error) {
	if cfg == nil {
		return metadata.Key{}, metadata.Scope{}, nil, fmt.Errorf("metadata target: connection config is nil")
	}
	if cfg.Driver == dialect.DatabaseDriverInterBase {
		return interBaseMetadataTarget(cfg)
	}
	return genericMetadataTarget(cfg, liveConn)
}

func interBaseMetadataTarget(cfg *database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, error) {
	target, err := database.InterBaseMetadataTargetFor(cfg)
	if err != nil {
		return metadata.Key{}, metadata.Scope{}, nil, err
	}
	ibCfg := ibmeta.Config{
		Target:             target.Target,
		User:               target.User,
		Password:           target.Password,
		Role:               target.Role,
		Charset:            target.Charset,
		CatalogTextCharset: target.CatalogTextCharset,
		Dialect:            target.Dialect,
		ConnectTimeout:     target.ConnectTimeout,
		TLS: ibmeta.TLSConfig{
			Enabled:              target.TLSEnabled,
			ServerPublicFile:     target.TLSServerPublicFile,
			ServerPublicPath:     target.TLSServerPublicPath,
			ClientCertFile:       target.TLSClientCertFile,
			ClientPassPhrase:     target.TLSClientPassPhrase,
			ClientPassPhraseFile: target.TLSClientPassPhraseFile,
		},
	}
	key, err := ibmeta.KeyFor(ibCfg)
	if err != nil {
		return metadata.Key{}, metadata.Scope{}, nil, err
	}
	scope, err := ibmeta.ScopeFor(ibCfg)
	if err != nil {
		return metadata.Key{}, metadata.Scope{}, nil, err
	}
	return key, scope, ibmeta.Factory(ibCfg), nil
}

const genericMetadataUnauthenticatedPrincipal = "sqls-generic-unauthenticated"

func genericMetadataTarget(cfg *database.DBConfig, liveConn func() *database.DBConnection) (metadata.Key, metadata.Scope, metadata.Factory, error) {
	key := metadata.Key{Engine: string(cfg.Driver), URL: genericMetadataURL(cfg)}
	principal := strings.TrimSpace(cfg.User)
	if principal == "" {
		principal = genericMetadataUnauthenticatedPrincipal
	}
	scope := metadata.Scope{Principal: principal, Role: "", TextPolicy: string(cfg.Driver)}
	driver := cfg.Driver
	factory := func(context.Context) (metadata.Provider, error) {
		return &genericMetadataProvider{driver: driver, liveConn: liveConn}, nil
	}
	return key, scope, factory, nil
}

// genericMetadataURL is credential-free whenever cfg carries structured
// connection fields (see the Key/Scope design note on metadataTargetFor).
func genericMetadataURL(cfg *database.DBConfig) string {
	if cfg.DataSourceName != "" {
		return string(cfg.Driver) + "|dsn=" + cfg.DataSourceName
	}
	return fmt.Sprintf("%s|proto=%s|host=%s|port=%d|path=%s|dbname=%s",
		cfg.Driver, cfg.Proto, cfg.Host, cfg.Port, cfg.Path, cfg.DBName)
}

// genericMetadataProvider adapts a DBRepository to metadata.Provider for
// every non-InterBase driver (Ruling R8). repo is used directly when set —
// bindMetadataRepoForTest and other test helpers construct a
// genericMetadataProvider this way, wrapping a repository they fully
// control. Production's genericMetadataTarget instead sets driver/liveConn:
// Plan() then builds a fresh repository from the host's CURRENT query
// connection (never a second pool) on every call, resolved dynamically
// rather than captured once at Factory-creation time — see
// metadataTargetFor's doc comment for why a static capture is unsafe across
// an explicit same-target Lease.Refresh. Close is a no-op either way: the
// underlying connection's lifecycle belongs to the host
// (enqueueDetachedConnection/cleanupConnections), never to the registry.
type genericMetadataProvider struct {
	repo database.DBRepository

	driver   dialect.DatabaseDriver
	liveConn func() *database.DBConnection
}

func (p *genericMetadataProvider) Plan() catalog.MetadataPlan {
	repo := p.repo
	if repo == nil && p.liveConn != nil {
		conn := p.liveConn()
		if conn == nil {
			return catalog.MetadataPlan{}
		}
		built, err := database.CreateRepositoryFromConnection(p.driver, conn)
		if err != nil {
			return catalog.MetadataPlan{}
		}
		repo = built
	}
	if repo == nil {
		return catalog.MetadataPlan{}
	}
	return database.MetadataPlanFor(repo)
}

func (p *genericMetadataProvider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{}, metadata.ErrUnsupported
}

func (p *genericMetadataProvider) Close() error { return nil }
