package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"golang.org/x/crypto/ssh"
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

func TestOpenContextCancellationClosesEveryLateConnectionResource(t *testing.T) {
	name := dialect.DatabaseDriver("test-open-context-close-all")
	started := make(chan struct{})
	release := make(chan struct{})
	var sqlCloses, sshCloses, tunnelCloses atomic.Int32
	driverConn := &recordingDriverConn{closes: &sqlCloses, err: errors.New("sql close failed")}
	sshErr := errors.New("ssh close failed")
	tunnelErr := errors.New("tunnel close failed")
	RegisterOpen(name, func(*DBConfig) (*DBConnection, error) {
		close(started)
		<-release
		db := sql.OpenDB(recordingConnector{conn: driverConn})
		pooled, err := db.Conn(context.Background())
		if err != nil {
			return nil, err
		}
		if err := pooled.Close(); err != nil {
			return nil, err
		}
		return &DBConnection{
			Conn: db,
			SSHConn: &ssh.Client{Conn: recordingSSHConn{
				closed: &sshCloses,
				err:    sshErr,
			}},
			Tunnel: closeFunc(func() error {
				tunnelCloses.Add(1)
				return tunnelErr
			}),
		}, nil
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
	for resource, got := range map[string]int32{
		"sql connection": sqlCloses.Load(),
		"ssh connection": sshCloses.Load(),
		"tunnel":         tunnelCloses.Load(),
	} {
		if got != 1 {
			t.Errorf("%s close count = %d, want 1", resource, got)
		}
	}
}

func TestDBConnectionCloseAttemptsAndJoinsEveryResourceError(t *testing.T) {
	connErr := errors.New("sql close failed")
	sshErr := errors.New("ssh close failed")
	tunnelErr := errors.New("tunnel close failed")
	var sshCloses, tunnelCloses atomic.Int32
	driverConn := &recordingDriverConn{closes: new(atomic.Int32), err: connErr}
	db := sql.OpenDB(recordingConnector{conn: driverConn})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}

	connection := &DBConnection{
		Conn: db,
		SSHConn: &ssh.Client{Conn: recordingSSHConn{
			closed: &sshCloses,
			err:    sshErr,
		}},
		Tunnel: closeFunc(func() error {
			tunnelCloses.Add(1)
			return tunnelErr
		}),
	}
	gotErr := connection.Close()
	for _, want := range []error{connErr, sshErr, tunnelErr} {
		if !errors.Is(gotErr, want) {
			t.Errorf("Close() error = %v, want it to wrap %v", gotErr, want)
		}
	}
	if got := sshCloses.Load(); got != 1 {
		t.Errorf("ssh close count = %d, want 1", got)
	}
	if got := tunnelCloses.Load(); got != 1 {
		t.Errorf("tunnel close count = %d, want 1", got)
	}
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

type recordingConnector struct{ conn driver.Conn }

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (recordingConnector) Driver() driver.Driver                          { return recordingDriver{} }

type recordingDriver struct{}

func (recordingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected Open call")
}

type recordingDriverConn struct {
	closes *atomic.Int32
	err    error
	mu     sync.Mutex
}

func (c *recordingDriverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare call")
}
func (c *recordingDriverConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes.Add(1)
	return c.err
}
func (*recordingDriverConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected Begin call")
}

type recordingSSHConn struct {
	ssh.Conn
	closed *atomic.Int32
	err    error
}

func (c recordingSSHConn) Close() error {
	c.closed.Add(1)
	return c.err
}
