package ibmeta

import (
	"context"
	"strings"
	"time"

	"github.com/sqls-server/sqls/metadata"
)

// TLSConfig is the caller-facing projection of the driver's TLS options.
// Passphrases here are factory inputs only: they are never derived into a
// Key, Scope or Event.
type TLSConfig struct {
	Enabled              bool
	ServerPublicFile     string
	ServerPublicPath     string
	ClientCertFile       string
	ClientPassPhrase     string
	ClientPassPhraseFile string
}

// Config is the explicit attachment configuration this package's Factory
// uses to build a store-owned InterBase metadata connection, independent of
// any query/execution connection a caller might separately hold. It has no
// dependency on LSP configuration types. Charset flows straight to the
// driver: unlike SQLs's own connections[] flow, this API does not validate
// it against interBaseCharsets, so a caller that already supports "NONE"
// (as MDExplorer's application attachments do) is never rejected here.
type Config struct {
	Target             string
	User               string
	Password           string
	Role               string
	Charset            string
	CatalogTextCharset string
	Dialect            int
	ConnectTimeout     time.Duration
	TLS                TLSConfig
}

// KeyFor delegates to NormalizeTarget so every caller of this package shares
// exactly one attachment-normalization path.
func KeyFor(cfg Config) (metadata.Key, error) {
	return NormalizeTarget(cfg.Target)
}

// ScopeFor derives the authenticated visibility this Config observes. An
// unauthenticated (blank) principal returns ErrScopeRequired: cached
// metadata coverage is permission-sensitive, and two connections cannot
// safely share a load without knowing who they authenticated as.
func ScopeFor(cfg Config) (metadata.Scope, error) {
	principal := strings.TrimSpace(cfg.User)
	if principal == "" {
		return metadata.Scope{}, metadata.ErrScopeRequired
	}
	return metadata.Scope{
		Principal:  principal,
		Role:       strings.TrimSpace(cfg.Role),
		TextPolicy: strings.TrimSpace(cfg.Charset) + "/" + strings.TrimSpace(cfg.CatalogTextCharset),
	}, nil
}

// Factory returns a metadata.Factory that opens a store-owned InterBase
// attachment for cfg on demand. The registry invokes it with a
// registry-owned context; canceling a caller's Acquire never cancels a
// Factory call another consumer still needs.
func Factory(cfg Config) metadata.Factory {
	return func(ctx context.Context) (metadata.Provider, error) {
		return newProvider(ctx, cfg)
	}
}
