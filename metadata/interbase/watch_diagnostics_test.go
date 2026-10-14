//go:build interbase && cgo && linux && amd64

package ibmeta

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
	interbase "interbase-go"
	"interbase-go/schema"
	"testing"
)

func TestWatchNativeDiagnosticBoundary(t *testing.T) {
	pool, e := sql.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	for _, native := range []bool{false, true} {
		secret := errors.New("interbase: native pw-secret /private/db SELECT ACL-secret")
		cause := error(secret)
		if native {
			cause = errors.Join(secret, &interbase.Error{Message: "pw-secret"})
		}
		f, m, e := readWatchedFingerprint(context.Background(), pool, 100, func(context.Context, *sql.Conn, int64) (*schema.CatalogFingerprint, error) { return nil, cause })
		if f != nil || e == nil || !m.DurationKnown || errors.Is(e, metadata.ErrNativeFailure) != native {
			t.Fatal(native, m, e)
		}
	}
	if e := classifyWatchNativeError(errors.Join(metadata.ErrWatchIncomplete, &interbase.Error{Message: "secret"})); !errors.Is(e, metadata.ErrWatchIncomplete) || !errors.Is(e, metadata.ErrNativeFailure) {
		t.Fatal(e)
	}
}

func TestWatchPartialFullMetricsForwarded(t *testing.T) {
	pool, e := sql.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	p := &nativeProvider{conn: pool, repo: &database.InterBaseDBRepository{Conn: pool, SQLDialect: 3}}
	cache, _, m, e := p.ReadFull(context.Background())
	if e == nil || cache != nil || !m.DurationKnown || !m.QueriesKnown || m.Queries < 1 || !errors.Is(e, metadata.ErrWatchIncomplete) {
		t.Fatal(cache, m, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, m, e = p.ReadFull(ctx)
	if !errors.Is(e, context.Canceled) || !m.DurationKnown || !m.QueriesKnown || m.Queries != 0 {
		t.Fatal(m, e)
	}
}
