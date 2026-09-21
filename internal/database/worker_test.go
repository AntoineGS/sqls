package database

import (
	"context"
	"testing"
	"time"
)

// newWorkerTestRepo builds a repository whose secondary cache pass (which the
// worker goroutine runs) can be parked, so the test can write w.dbRepo from the
// caller goroutine while the worker goroutine is reading it.
//
// `entered` is buffered and the parked pass sends on it without anyone
// receiving: the send must never block, because nothing reads it.
func newWorkerTestRepo(describeAll func(context.Context) ([]*ColumnDesc, error)) *MockDBRepository {
	return &MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"t"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*ColumnDesc, error) {
			return nil, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*ForeignKey, error) {
			return nil, nil
		},
		MockDescribeDatabaseTable: describeAll,
	}
}

func TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates(t *testing.T) {
	ctx := context.Background()

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	parked := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		entered <- struct{}{}
		<-release
		return nil, nil
	})
	fast := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		return nil, nil
	})

	w := NewWorker()
	w.Start()
	t.Cleanup(w.Stop)
	t.Cleanup(func() { close(release) })

	if err := w.ReCache(ctx, parked); err != nil {
		t.Fatal("first ReCache:", err)
	}

	// Deliberately a sleep, not a receive on `entered`. The worker goroutine
	// reads w.dbRepo at worker.go:58 *before* it calls the gated method, so
	// receiving the gate's signal would place that read in this goroutine's
	// happens-before history and the detector would consider the write below
	// ordered after it. A sleep leaves the two accesses unordered, which is
	// what the race actually is. Verified: with a receive here the test passes
	// against unfixed code; with this sleep it reports the race.
	time.Sleep(200 * time.Millisecond)

	if err := w.ReCache(ctx, fast); err != nil {
		t.Fatal("second ReCache:", err)
	}

	if w.Cache() == nil {
		t.Fatal("worker cache is nil after ReCache")
	}
}

func TestWorkerStopIsIdempotent(t *testing.T) {
	w := NewWorker()
	w.Start()
	w.Stop()
	w.Stop()
}
