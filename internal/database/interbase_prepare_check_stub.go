//go:build !interbase || !cgo || !(linux || windows) || !amd64

package database

import (
	"context"
	"database/sql"
	"errors"
)

func PrepareCheckInterBase(context.Context, *sql.DB, string) (bool, int, string, error) {
	return false, 0, "", errors.New("interbase: native driver not built")
}
