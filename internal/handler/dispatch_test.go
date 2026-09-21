package handler

import (
	"errors"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestStopStopsWorkerEvenWhenConnectionCloseFails(t *testing.T) {
	closeErr := errors.New("attachment is half dead")
	server := NewServer()
	server.dbConn = &database.DBConnection{
		Driver: "stub",
		Tunnel: failingCloser{err: closeErr},
	}

	if err := server.Stop(); !errors.Is(err, closeErr) {
		t.Fatalf("Stop() = %v, want %v", err, closeErr)
	}

	// A stopped worker must not accept another update. Stop is idempotent, so
	// calling it again is the cheap way to assert it already ran: a worker that
	// was never stopped would still be running here and the select below would
	// not see a closed channel.
	select {
	case <-server.worker.Done():
	default:
		t.Fatal("Stop returned without stopping the worker")
	}
}
