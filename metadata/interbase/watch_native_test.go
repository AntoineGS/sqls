//go:build interbase && cgo && linux && amd64

package ibmeta

import (
	"context"
	"database/sql"
	"errors"
	_ "github.com/mattn/go-sqlite3"
	"github.com/sqls-server/sqls/metadata"
	interbase "interbase-go"
	"interbase-go/schema"
	"os"
	"testing"
	"time"
)

func TestWatchedFingerprintEqualityIgnoresReadTiming(t *testing.T) {
	left := &watchedFingerprint{value: &schema.CatalogFingerprint{Version: "ib-catalog-v1", Stats: schema.CatalogFingerprintStats{Rows: 1}}}
	right := &watchedFingerprint{value: &schema.CatalogFingerprint{Version: "ib-catalog-v1", Stats: schema.CatalogFingerprintStats{Rows: 2}}}
	if !left.Equal(right) {
		t.Fatal("metrics must not cause catalog change")
	}
	right.value.Version = "ib-catalog-v2"
	if left.Equal(right) {
		t.Fatal("different projection formats are unequal")
	}
	left.value.AccountedBytes = 123
	if left.Format() != "ib-catalog-v1" || left.RetainedBytes() != 123 {
		t.Fatal("wrapper lost format/accounting")
	}
	if left.Equal(foreignFingerprint{}) {
		t.Fatal("foreign wrapper accepted")
	}
}

type foreignFingerprint struct{}

func (foreignFingerprint) Format() string                  { return "ib-catalog-v1" }
func (foreignFingerprint) RetainedBytes() int64            { return 123 }
func (foreignFingerprint) Equal(metadata.Fingerprint) bool { return true }

func TestWatchedFingerprintMetricsAndPoolRelease(t *testing.T) {
	pool, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	value := &schema.CatalogFingerprint{Version: "ib-catalog-v1", AccountedBytes: 123, Stats: schema.CatalogFingerprintStats{Queries: 7, Rows: 9, BlobOpens: 2, SourceBytes: 44}}
	for range 2 {
		f, m, err := readWatchedFingerprint(context.Background(), pool, 456, func(_ context.Context, c *sql.Conn, limit int64) (*schema.CatalogFingerprint, error) {
			if c == nil || limit != 456 || pool.Stats().InUse != 1 {
				t.Fatal("scanner did not borrow provider pool")
			}
			return value, nil
		})
		if err != nil || f.Format() != value.Version || m.Queries != 7 || m.Rows != 9 || m.BlobOpens != 2 || m.SourceBytes != 44 || m.RetainedBytes != 123 || m.Duration <= 0 {
			t.Fatalf("fingerprint=%v metrics=%+v err=%v", f, m, err)
		}
		if pool.Stats().InUse != 0 || pool.Stats().OpenConnections != 1 {
			t.Fatal("connection was not released/reused")
		}
	}
}

func TestWatchedFingerprintNeutralErrors(t *testing.T) {
	pool, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, tc := range []struct {
		limit int64
		want  error
	}{{0, metadata.ErrWatchLimit}, {64 << 20, metadata.ErrWatchCapability}} {
		f, _, err := readWatchedFingerprint(context.Background(), pool, tc.limit, interbase.ReadCatalogFingerprint)
		if f != nil || !errors.Is(err, tc.want) || pool.Stats().InUse != 0 {
			t.Fatalf("fingerprint=%v err=%v want=%v", f, err, tc.want)
		}
	}
}

func TestWatchedFingerprintCancellationAtScannerCompletion(t *testing.T) {
	pool, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, _, err := readWatchedFingerprint(ctx, pool, 100, func(context.Context, *sql.Conn, int64) (*schema.CatalogFingerprint, error) {
		cancel()
		return &schema.CatalogFingerprint{Version: "ib-catalog-v1"}, nil
	})
	if f != nil || !errors.Is(err, context.Canceled) || pool.Stats().InUse != 0 {
		t.Fatalf("completed canceled scan f=%v err=%v", f, err)
	}
}

func TestWatchedFingerprintCancellationWaitsScannerDrain(t *testing.T) {
	pool, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := readWatchedFingerprint(ctx, pool, 100, func(ctx context.Context, _ *sql.Conn, _ int64) (*schema.CatalogFingerprint, error) {
			close(entered)
			<-release
			return nil, ctx.Err()
		})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if pool.Stats().InUse != 1 {
		t.Fatal("pool released before scanner drain")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if pool.Stats().InUse != 0 {
		t.Fatal("pool not released after drain")
	}
}

func TestWatchedFingerprintDisposableNative(t *testing.T) {
	if os.Getenv("SQLS_WATCH_DISPOSABLE") != "1" {
		t.Skip("disposable fixture wrapper required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := newProvider(ctx, Config{Target: os.Getenv("SQLS_WATCH_DISPOSABLE_DATABASE"), User: os.Getenv("SQLS_WATCH_DISPOSABLE_USER"), Password: os.Getenv("SQLS_WATCH_DISPOSABLE_PASSWORD"), Charset: "NONE", CatalogTextCharset: "WIN1252", Dialect: 3})
	if err != nil {
		t.Fatal("disposable attach failed")
	}
	defer provider.Close()
	p := provider.(*nativeProvider)
	p.conn.SetMaxOpenConns(1)
	left, lm, err := p.ReadFingerprint(ctx, 64<<20)
	if err != nil {
		t.Fatal("native fingerprint failed")
	}
	right, rm, err := p.ReadFingerprint(ctx, 64<<20)
	if err != nil {
		t.Fatal("second native fingerprint failed")
	}
	if !left.Equal(right) || left.Format() != left.(*watchedFingerprint).value.Version || lm.Queries <= 0 || lm.Rows <= 0 || lm.BlobOpens != 0 || lm.SourceBytes != 0 || lm.RetainedBytes != left.RetainedBytes() || rm.RetainedBytes != right.RetainedBytes() || p.conn.Stats().InUse != 0 {
		t.Fatal("native wrapper metrics/equality/pool lifecycle failed")
	}
	f, _, err := p.ReadFingerprint(ctx, left.RetainedBytes()-1)
	if f != nil || !errors.Is(err, metadata.ErrWatchLimit) || p.conn.Stats().InUse != 0 {
		t.Fatal("native limit/pool release failed")
	}
	t.Logf("native fingerprint format=%s queries=%d rows=%d blob-opens=%d source-bytes=%d retained-bytes=%d", left.Format(), lm.Queries, lm.Rows, lm.BlobOpens, lm.SourceBytes, lm.RetainedBytes)
}
