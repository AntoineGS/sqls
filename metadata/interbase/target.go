// Package ibmeta normalizes InterBase attachment strings into shared
// metadata registry keys. Its Go package name is ibmeta, distinct from its
// directory name metadata/interbase, so consumers import it as
// ibmeta "github.com/sqls-server/sqls/metadata/interbase" to tell it apart
// from the interbase-go driver.
package ibmeta

import (
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/metadata"
)

const (
	engine = "interbase"
	scheme = "ib://"
)

// NormalizeTarget derives a metadata.Key from a raw InterBase attachment
// string. It recognizes and strips a case-insensitive "ib://" prefix, then
// reapplies the canonical lower-case prefix; every other byte of the
// attachment (host, port, path, case, "@", backslashes) is preserved exactly,
// so an "@" in the attachment is never treated as a credential delimiter.
// This increment does not merge host aliases or default-port equivalents:
// only the scheme-prefix rule is applied.
func NormalizeTarget(raw string) (metadata.Key, error) {
	rest := raw
	if len(rest) >= len(scheme) && strings.EqualFold(rest[:len(scheme)], scheme) {
		rest = rest[len(scheme):]
	}
	if strings.TrimSpace(rest) == "" {
		return metadata.Key{}, fmt.Errorf("%w: blank InterBase target", metadata.ErrInvalidTarget)
	}
	return metadata.Key{Engine: engine, URL: scheme + rest}, nil
}
