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

func TestWorkerDiscardsSecondaryColumnsFromPreviousRepository(t *testing.T) {
	oldSecondaryStarted := make(chan struct{}, 1)
	newSecondaryStarted := make(chan struct{}, 1)
	oldSecondaryRelease := make(chan struct{}, 1)
	newSecondaryRelease := make(chan struct{}, 1)
	oldColumns := []*ColumnDesc{{ColumnBase: ColumnBase{Table: "T", Name: "OLD"}, Type: "VARCHAR(40)"}}
	newColumns := []*ColumnDesc{{ColumnBase: ColumnBase{Table: "T", Name: "NEW"}, Type: "VARCHAR(20)"}}

	oldRepo := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		oldSecondaryStarted <- struct{}{}
		<-oldSecondaryRelease
		return oldColumns, nil
	})
	oldRepo.MockDescribeDatabaseTableBySchema = func(context.Context, string) ([]*ColumnDesc, error) {
		return oldColumns, nil
	}
	newRepo := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		newSecondaryStarted <- struct{}{}
		<-newSecondaryRelease
		return newColumns, nil
	})
	newRepo.MockDescribeDatabaseTableBySchema = func(context.Context, string) ([]*ColumnDesc, error) {
		return newColumns, nil
	}

	w := NewWorker()
	w.Start()
	t.Cleanup(func() {
		select {
		case oldSecondaryRelease <- struct{}{}:
		default:
		}
		select {
		case newSecondaryRelease <- struct{}{}:
		default:
		}
		w.Stop()
	})

	if err := w.ReCache(context.Background(), oldRepo); err != nil {
		t.Fatal("first ReCache:", err)
	}
	select {
	case <-oldSecondaryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("old repository secondary pass did not start")
	}

	if err := w.ReCache(context.Background(), newRepo); err != nil {
		t.Fatal("second ReCache:", err)
	}
	oldSecondaryRelease <- struct{}{}
	select {
	case <-newSecondaryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("new repository secondary pass did not start")
	}

	cache := w.Cache()
	if _, ok := cache.Column("T", "NEW"); !ok {
		t.Fatal("new primary columns were replaced by the previous repository's secondary result")
	}
	if _, ok := cache.Column("T", "OLD"); ok {
		t.Fatal("previous repository column leaked into the new connection cache")
	}
}

func TestWorkerStopIsIdempotent(t *testing.T) {
	w := NewWorker()
	w.Start()
	w.Stop()
	w.Stop()
}
