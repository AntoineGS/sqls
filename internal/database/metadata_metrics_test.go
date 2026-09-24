package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetadataMetrics(t *testing.T) {
	t.Run("successful jobs expose queue and run durations", func(t *testing.T) {
		queued := time.Unix(100, 0)
		started := queued.Add(25 * time.Millisecond)
		finished := started.Add(40 * time.Millisecond)
		metrics := metadataJobMetrics(MetadataStatus{
			State: MetadataReady, QueuedAt: queued, StartedAt: started, FinishedAt: finished,
			Count: 7, Queries: 3, QueriesKnown: true,
		})
		if metrics.Queue.Milliseconds() != 25 || metrics.Run.Milliseconds() != 40 {
			t.Fatalf("durations = %+v, want queue=25ms run=40ms", metrics)
		}
		if !metrics.QueriesKnown || metrics.Queries != 3 || metrics.Count != 7 {
			t.Fatalf("metrics = %+v, lost successful metrics", metrics)
		}
	})

	t.Run("failed jobs retain query counts", func(t *testing.T) {
		loader := NewMetadataLoader()
		t.Cleanup(loader.Stop)
		loader.Reset(1)
		load, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{
			Parallelism: 1,
			Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				return MetadataPatch{Count: 9, Queries: 2, QueriesKnown: true}, errors.New("failed after queries")
			}}},
		}))
		if err != nil {
			t.Fatal(err)
		}
		waitLoad(t, load)
		status := loader.Snapshot().Status[MetadataViews]
		if !status.QueriesKnown || status.Queries != 2 {
			t.Fatalf("lost failed-job counts: %+v", status)
		}
		if status.State != MetadataFailed {
			t.Fatalf("failure recorded as %s", status.State)
		}
		if metrics := metadataJobMetrics(status); metrics.SuccessfulCount != 0 || metrics.FailedCount != 9 {
			t.Fatalf("failure count was not kept separate: %+v", metrics)
		}
	})

	t.Run("cancellation while queued has zero run duration", func(t *testing.T) {
		loader := NewMetadataLoader()
		loader.Reset(1)
		for i := 0; i < cap(loader.semaphore); i++ {
			loader.semaphore <- struct{}{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		load, err := loader.Start(ctx, 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{
			Kind: MetadataViews,
			Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				t.Fatal("queued job unexpectedly started")
				return MetadataPatch{}, nil
			},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		waitLoad(t, load)
		status := loader.Snapshot().Status[MetadataViews]
		if status.State != MetadataCancelled || !status.StartedAt.IsZero() {
			t.Fatalf("queued cancellation status = %+v", status)
		}
		if got := metadataJobMetrics(status).Run; got != 0 {
			t.Fatalf("queued cancellation run duration = %s, want zero", got)
		}
		loader.Stop()
	})

	t.Run("cancelled running job retains metrics after native call drains", func(t *testing.T) {
		loader := NewMetadataLoader()
		t.Cleanup(loader.Stop)
		gate := make(chan struct{})
		started := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loader.Reset(1)
		load, err := loader.Start(ctx, 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{
			Kind: MetadataViews,
			Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				close(started)
				<-gate
				return MetadataPatch{
					Cache: &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{"cancelled-result": {Name: "cancelled-result"}}}},
					Count: 6, Queries: 2, QueriesKnown: true,
				}, context.Canceled
			},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		<-started
		cancel()
		waitLoad(t, load)
		if got := loader.Snapshot().Status[MetadataViews].State; got != MetadataCancelled {
			t.Fatalf("logical state = %s, want cancelled", got)
		}
		close(gate)
		if err := loader.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		status := loader.Snapshot().Status[MetadataViews]
		if status.State != MetadataCancelled || status.Count != 6 || !status.QueriesKnown || status.Queries != 2 {
			t.Fatalf("drained cancelled job lost metrics: %+v", status)
		}
		if _, ok := loader.Cache().View("cancelled-result"); ok {
			t.Fatal("cancelled job published its fragment")
		}
	})

	t.Run("admitted but never invoked job has no start timestamp", func(t *testing.T) {
		loader := NewMetadataLoader()
		t.Cleanup(loader.Stop)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loader.Reset(1)
		loader.SetChangedCallback(func() {
			status := loader.Snapshot().Status[MetadataViews]
			if status.State == MetadataLoading {
				cancel()
			}
		})
		load, err := loader.Start(ctx, 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{
			Kind: MetadataViews,
			Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				t.Fatal("cancelled-before-invocation job ran")
				return MetadataPatch{}, nil
			},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		waitLoad(t, load)
		status := loader.Snapshot().Status[MetadataViews]
		if status.State != MetadataCancelled || !status.StartedAt.IsZero() || metadataJobMetrics(status).Run != 0 {
			t.Fatalf("never-invoked job has false run timing: %+v", status)
		}
	})

	t.Run("superseded generation does not contribute to current summary", func(t *testing.T) {
		loader := NewMetadataLoader()
		t.Cleanup(loader.Stop)
		gate := make(chan struct{})
		loader.Reset(1)
		old, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{
			Kind: MetadataViews,
			Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				<-gate
				return MetadataPatch{Count: 5, Queries: 2, QueriesKnown: true}, nil
			},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		loader.Reset(2)
		current, err := loader.Start(context.Background(), 2, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{
			Kind: MetadataRelations,
			Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				return MetadataPatch{Count: 1, Queries: 1, QueriesKnown: true}, nil
			},
		}}}))
		if err != nil {
			t.Fatal(err)
		}
		close(gate)
		waitLoad(t, old)
		waitLoad(t, current)
		summary := metadataGenerationMetrics(loader.Snapshot())
		if summary.Generation != 2 || summary.Count != 1 || summary.Queries != 1 {
			t.Fatalf("current-generation summary = %+v", summary)
		}
	})

	t.Run("generic repositories mark query count unknown", func(t *testing.T) {
		loader := NewMetadataLoader()
		t.Cleanup(loader.Stop)
		loader.Reset(1)
		load, err := loader.Start(context.Background(), 1, NewMockDBRepository(nil))
		if err != nil {
			t.Fatal(err)
		}
		waitLoad(t, load)
		status := loader.Snapshot().Status[MetadataRelations]
		if status.QueriesKnown || status.Queries != 0 {
			t.Fatalf("generic query accounting = %+v, want unknown", status)
		}
	})
}
