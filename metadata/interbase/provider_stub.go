//go:build !interbase || !cgo || !(linux || windows) || !amd64

package ibmeta

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/metadata"
)

func newProvider(context.Context, Config) (metadata.Provider, error) {
	return nil, errors.New("ibmeta: native InterBase provider unavailable; rebuild on linux/amd64 or windows/amd64 with CGO_ENABLED=1 and the interbase build tag (-tags interbase)")
}
