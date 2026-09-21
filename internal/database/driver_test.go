package database

import (
	"errors"
	"testing"
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
