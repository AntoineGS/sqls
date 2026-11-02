package duckmeta

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

func TestFactoryReturnsOpenError(t *testing.T) {
	wantErr := errors.New("host registry unavailable")
	factory := Factory(Config{Target: "/tmp/test.duckdb"}, func(context.Context) (ConnSource, error) {
		return nil, wantErr
	})
	provider, err := factory(context.Background())
	if provider != nil {
		t.Fatalf("Factory() provider = %T, want nil", provider)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Factory() error = %v, want wrapped %v", err, wantErr)
	}
}

func TestProviderCloseClosesUnusedSourceOnce(t *testing.T) {
	source := newCountingDuckDBSource(t)
	factory := Factory(Config{Target: "/tmp/test.duckdb"}, func(context.Context) (ConnSource, error) {
		return source, nil
	})
	provider, err := factory(context.Background())
	if err != nil {
		t.Fatalf("Factory(): %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("first Close(): %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
	if got := source.closes.Load(); got != 1 {
		t.Fatalf("ConnSource.Close calls = %d, want 1", got)
	}
}

func TestPlanJobWrapsConnSourceFailure(t *testing.T) {
	source := newCountingDuckDBSource(t)
	wantErr := errors.New("host connection is closed")
	source.failAt = 2 // Factory reads current_database() on the first connection.
	source.connErr = wantErr
	factory := Factory(Config{Target: "/tmp/test.duckdb"}, func(context.Context) (ConnSource, error) {
		return source, nil
	})
	provider, err := factory(context.Background())
	if err != nil {
		t.Fatalf("Factory(): %v", err)
	}
	defer provider.Close()

	plan := provider.Plan()
	if len(plan.Jobs) == 0 {
		t.Fatal("Plan() has no jobs")
	}
	_, err = plan.Jobs[0].Run(context.Background(), nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("first metadata job error = %v, want wrapped %v", err, wantErr)
	}
}

type countingDuckDBSource struct {
	db      *sql.DB
	calls   atomic.Int64
	closes  atomic.Int64
	failAt  int64
	connErr error
}

func newCountingDuckDBSource(t *testing.T) *countingDuckDBSource {
	t.Helper()
	db, err := sql.Open("duckdb", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(duckdb): %v", err)
	}
	db.SetMaxOpenConns(1)
	return countingDuckDBSourceForDB(t, db)
}

func countingDuckDBSourceForDB(t *testing.T, db *sql.DB) *countingDuckDBSource {
	t.Helper()
	t.Cleanup(func() { _ = db.Close() })
	return &countingDuckDBSource{db: db}
}

func (s *countingDuckDBSource) Conn(ctx context.Context) (*sql.Conn, error) {
	call := s.calls.Add(1)
	if call == s.failAt {
		return nil, s.connErr
	}
	return s.db.Conn(ctx)
}

func (s *countingDuckDBSource) Close() error {
	s.closes.Add(1)
	return s.db.Close()
}

var _ ConnSource = (*countingDuckDBSource)(nil)
