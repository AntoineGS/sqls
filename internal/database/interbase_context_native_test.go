//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	interbase "interbase-go"
)

func TestInterBaseAttachContextHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := interBaseAttachContext(ctx, interBaseConnConfig{Database: "unused", User: "SYSDBA", Charset: "UTF8"}, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interBaseAttachContext() error = %v, want context.Canceled", err)
	}
}

func TestInterBaseDiagnosticsContextHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := sql.OpenDB(nil)
	defer conn.Close()
	_, err := interBaseDiagnosticsContext(ctx, conn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interBaseDiagnosticsContext() error = %v, want context.Canceled", err)
	}
}

func TestInterBaseOpenContextDoesNotReattachAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attachCalls := 0
	_, err := interBaseOpenContextWith(ctx, &DBConfig{
		Driver: dialect.DatabaseDriverInterBase,
		DBName: "unused",
		User:   "SYSDBA",
	}, func(context.Context, interBaseConnConfig, int) (*sql.DB, error) {
		attachCalls++
		return sql.OpenDB(nil), nil
	}, func(context.Context, *sql.DB) (interbase.DatabaseDiagnostics, error) {
		cancel()
		return interbase.DatabaseDiagnostics{SQLDialect: 1}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interBaseOpenContext() error = %v, want context.Canceled", err)
	}
	if attachCalls != 1 {
		t.Fatalf("attach calls = %d, want 1 (no dialect-1 reattach)", attachCalls)
	}
}
