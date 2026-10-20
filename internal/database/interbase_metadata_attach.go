//go:build interbase && cgo && (linux || windows) && amd64

package database

import (
	"context"
	"database/sql"
	"time"
)

// InterBaseMetadataAttachConfig is the exported projection the shared
// metadata provider (metadata/interbase) uses to open its own attachment,
// independent of any query/execution connection a caller might separately
// hold. Unlike DBConfig-driven attachment, Charset is applied directly: it
// does not pass through interBaseCharset's allowlist, which exists only to
// validate SQLs's own configured connections[] flow and must not reject a
// mode (such as "NONE") that a shared-metadata caller already supports.
type InterBaseMetadataAttachConfig struct {
	Database           string
	Host               string
	User               string
	Password           string
	Role               string
	Charset            string
	CatalogTextCharset string
	ConnectTimeout     time.Duration

	TLSEnabled              bool
	TLSServerPublicFile     string
	TLSServerPublicPath     string
	TLSClientCertFile       string
	TLSClientPassPhrase     string
	TLSClientPassPhraseFile string
}

// AttachInterBaseForMetadata opens a pooled InterBase attachment for the
// shared metadata provider. It never validates cfg.Charset against
// interBaseCharsets: that allowlist is a legacy guard for SQLs's own
// connections[] configuration flow, not for this generic attachment path.
func AttachInterBaseForMetadata(ctx context.Context, cfg InterBaseMetadataAttachConfig, sqlDialect int) (*sql.DB, error) {
	connCfg := interBaseConnConfig{
		Database:           cfg.Database,
		Host:               cfg.Host,
		User:               cfg.User,
		Password:           cfg.Password,
		Role:               cfg.Role,
		Charset:            cfg.Charset,
		CatalogTextCharset: cfg.CatalogTextCharset,
		ConnectTimeout:     cfg.ConnectTimeout,
		TLS: interBaseTLSSettings{
			Enabled:              cfg.TLSEnabled,
			ServerPublicFile:     cfg.TLSServerPublicFile,
			ServerPublicPath:     cfg.TLSServerPublicPath,
			ClientCertFile:       cfg.TLSClientCertFile,
			ClientPassPhrase:     cfg.TLSClientPassPhrase,
			ClientPassPhraseFile: cfg.TLSClientPassPhraseFile,
		},
	}
	return interBaseAttachContext(ctx, connCfg, sqlDialect)
}
