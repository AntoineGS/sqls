package database

import (
	"time"

	"github.com/sqls-server/sqls/catalog"
)

// MetadataPlanFor is the exported-in-internal-package adaptation of
// metadataPlanFor: it lets callers outside this package (the future LSP host,
// and later tasks) build a catalog.MetadataPlan from a DBRepository without
// reaching into sqls internals.
func MetadataPlanFor(repo DBRepository) catalog.MetadataPlan {
	return metadataPlanFor(repo)
}

// InterBaseMetadataTarget is the validated, credential-bearing projection of a
// DBConfig used to derive a shared-metadata Key/Scope (via metadata/interbase)
// and to open a store-owned InterBase attachment for it. It reuses exactly the
// same field-by-field validation interBaseConnectionConfig applies for the
// query connection (attachment string, charset allowlist, catalog text
// charset, role length, connect timeout, TLS options) so the shared-metadata
// path can never silently diverge from the query connection's own rules.
type InterBaseMetadataTarget struct {
	Target             string
	User               string
	Password           string
	Role               string
	Charset            string
	CatalogTextCharset string
	Dialect            int
	ConnectTimeout     time.Duration

	TLSEnabled              bool
	TLSServerPublicFile     string
	TLSServerPublicPath     string
	TLSClientCertFile       string
	TLSClientPassPhrase     string
	TLSClientPassPhraseFile string
}

// InterBaseMetadataTargetFor derives cfg's InterBaseMetadataTarget. It never
// opens a connection; it only validates and projects cfg, the same way
// interBaseConnectionConfig does for the query path, so it builds cleanly on
// an untagged (no cgo) build.
func InterBaseMetadataTargetFor(cfg *DBConfig) (InterBaseMetadataTarget, error) {
	attachment, err := interBaseAttachment(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	catalogTextCharset, err := interBaseCatalogTextCharset(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	role, err := interBaseRole(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	connectTimeout, err := interBaseConnectTimeout(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	tls, err := interBaseTLS(cfg)
	if err != nil {
		return InterBaseMetadataTarget{}, err
	}
	return InterBaseMetadataTarget{
		Target:                  attachment,
		User:                    cfg.User,
		Password:                cfg.Passwd,
		Role:                    role,
		Charset:                 charset,
		CatalogTextCharset:      catalogTextCharset,
		Dialect:                 cfg.Dialect,
		ConnectTimeout:          connectTimeout,
		TLSEnabled:              tls.Enabled,
		TLSServerPublicFile:     tls.ServerPublicFile,
		TLSServerPublicPath:     tls.ServerPublicPath,
		TLSClientCertFile:       tls.ClientCertFile,
		TLSClientPassPhrase:     tls.ClientPassPhrase,
		TLSClientPassPhraseFile: tls.ClientPassPhraseFile,
	}, nil
}
