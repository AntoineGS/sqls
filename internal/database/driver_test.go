package database

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

type errCloser struct{ err error }

func (c errCloser) Close() error { return c.err }

func TestDBConnectionCloseWithoutSQLConn(t *testing.T) {
	conn := &DBConnection{Driver: "stub"}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

func TestDBConnectionCloseReportsTunnelFailure(t *testing.T) {
	want := errors.New("tunnel is wedged")
	conn := &DBConnection{Driver: "stub", Tunnel: errCloser{err: want}}
	if err := conn.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() = %v, want %v", err, want)
	}
}

func TestOpenContextSkipsOpenerWhenAlreadyCanceled(t *testing.T) {
	name := dialect.DatabaseDriver("test-open-context-canceled")
	var calls atomic.Int32
	RegisterOpenContext(name, func(context.Context, *DBConfig) (*DBConnection, error) {
		calls.Add(1)
		return &DBConnection{}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenContext(ctx, &DBConfig{Driver: name}); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenContext() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("opener calls = %d, want 0", got)
	}
}

func TestOpenContextClosesLegacyResultWhenCanceledDuringOpen(t *testing.T) {
	name := dialect.DatabaseDriver("test-open-context-legacy-cancel")
	started := make(chan struct{})
	release := make(chan struct{})
	var closes atomic.Int32
	RegisterOpen(name, func(*DBConfig) (*DBConnection, error) {
		close(started)
		<-release
		return &DBConnection{Tunnel: closeFunc(func() error {
			closes.Add(1)
			return nil
		})}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := OpenContext(ctx, &DBConfig{Driver: name})
		result <- err
	}()
	<-started
	cancel()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenContext() error = %v, want context.Canceled", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("late connection close count = %d, want 1", got)
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }
