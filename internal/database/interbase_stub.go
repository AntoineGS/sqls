//go:build !interbase || !cgo || !linux || !amd64

package database

import (
	"context"
	"errors"
)

func interBaseOpen(*DBConfig) (*DBConnection, error) {
	return nil, errors.New("interbase: native adapter unavailable; rebuild on linux/amd64 with CGO_ENABLED=1 and the interbase build tag (-tags interbase)")
}

func interBaseOpenContext(ctx context.Context, cfg *DBConfig) (*DBConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return interBaseOpen(cfg)
}
