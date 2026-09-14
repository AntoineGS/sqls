//go:build !interbase || !cgo || !linux || !amd64

package database

import "errors"

func interBaseOpen(*DBConfig) (*DBConnection, error) {
	return nil, errors.New("interbase: native adapter unavailable; rebuild on linux/amd64 with CGO_ENABLED=1 and the interbase build tag (-tags interbase)")
}
