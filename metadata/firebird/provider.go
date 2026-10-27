package fbmeta

import (
	"context"
	"strings"
	"time"

	"github.com/sqls-server/sqls/metadata"
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

// Factory returns a metadata.Factory for cfg. It reports ErrUnsupported until
// Task 4 supplies the Firebird provider implementation.
func Factory(cfg Config) metadata.Factory {
	return func(context.Context) (metadata.Provider, error) {
		return nil, metadata.ErrUnsupported
	}
}
