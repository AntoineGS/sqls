//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sqls-server/sqls/dialect"
	interbase "interbase-go"
)

const interBasePingTimeout = 10 * time.Second

func interBaseOpen(cfg *DBConfig) (*DBConnection, error) {
	if cfg == nil {
		return nil, errors.New("interbase: connection config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	attachment, err := interBaseAttachment(cfg)
	if err != nil {
		return nil, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := interbase.NewConnector(interbase.Config{
		Database: attachment,
		User:     cfg.User,
		Password: cfg.Passwd,
		Charset:  charset,
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: create connector: %w", err)
	}

	conn := sql.OpenDB(connector)
	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	ctx, cancel := context.WithTimeout(context.Background(), interBasePingTimeout)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("interbase: ping failed: %w", err)
	}

	return &DBConnection{Conn: conn, Driver: dialect.DatabaseDriverInterBase}, nil
}
