package database

import (
	"context"
	"errors"
	"testing"
	"time"
)

type metadataPlanTestRepo struct {
	*MockDBRepository
	plan MetadataPlan
}

func (r metadataPlanTestRepo) MetadataPlan() MetadataPlan { return r.plan }

func metadataRepo(plan MetadataPlan) metadataPlanTestRepo {
	return metadataPlanTestRepo{MockDBRepository: &MockDBRepository{}, plan: plan}
}

type metadataPlanFuncRepo struct {
	*MockDBRepository
	plan func() MetadataPlan
}

func (r metadataPlanFuncRepo) MetadataPlan() MetadataPlan { return r.plan() }

func waitLoad(t *testing.T, load *MetadataLoad) {
	t.Helper()
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("stranded load")
	}
}

func TestMetadataLoaderUsesGenericPlanWhenAbsent(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, NewMockDBRepository(nil))
	if err != nil {
		t.Fatalf("Start() with generic repository error = %v", err)
	}
	waitLoad(t, load)
	snapshot := loader.Snapshot()
	if !snapshot.Settled() {
		t.Fatalf("generic metadata snapshot did not settle: %#v", snapshot.Status)
	}
	for _, kind := range []MetadataKind{MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataColumnsAll, MetadataForeignKeys} {
		if got := snapshot.Status[kind].State; got != MetadataReady {
			t.Errorf("generic %s state = %s, want ready", kind, got)
		}
	}
	if _, ok := snapshot.Cache.Database("world"); !ok {
		t.Error("generic schemas were not published")
	}
	if _, ok := snapshot.Cache.ColumnDatabase("world", "city"); !ok {
		t.Error("generic columns were not published")
	}
}

func TestMetadataLoaderPlannerDoesNotBlockReset(t *testing.T) {
	loader := NewMetadataLoader()
	loader.Reset(1)
	entered, release := make(chan struct{}), make(chan struct{})
	startDone := make(chan error, 1)
	go func() {
		_, err := loader.Start(context.Background(), 1, metadataPlanFuncRepo{MockDBRepository: &MockDBRepository{}, plan: func() MetadataPlan {
			close(entered)
			<-release
			return MetadataPlan{Parallelism: 1}
		}})
		startDone <- err
	}()
	<-entered
	resetDone := make(chan struct{})
	go func() { loader.Reset(2); close(resetDone) }()
	select {
	case <-resetDone:
	case <-time.After(2 * time.Second):
		close(release)
		<-startDone
		t.Fatal("Reset blocked behind repository MetadataPlan")
	}
	close(release)
	if err := <-startDone; !errors.Is(err, ErrMetadataStaleGeneration) {
		t.Fatalf("Start error after concurrent Reset = %v", err)
	}
	loader.Stop()
}

func TestMetadataLoaderPlannerCanReenterSnapshot(t *testing.T) {
	loader := NewMetadataLoader()
	loader.Reset(1)
	startDone := make(chan error, 1)
	go func() {
		_, err := loader.Start(context.Background(), 1, metadataPlanFuncRepo{MockDBRepository: &MockDBRepository{}, plan: func() MetadataPlan {
			if got := loader.Snapshot().Generation; got != 1 {
				t.Errorf("planner snapshot generation = %d", got)
			}
			return MetadataPlan{Parallelism: 1}
		}})
		startDone <- err
	}()
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant Snapshot deadlocked in MetadataPlan")
	}
	loader.Stop()
}

func TestMetadataLoaderConcurrentStartsAcceptAtMostOne(t *testing.T) {
	loader := NewMetadataLoader()
	loader.Reset(1)
	const contenders = 8
	ready, release := make(chan struct{}, contenders), make(chan struct{})
	results := make(chan error, contenders)
	repo := metadataPlanFuncRepo{MockDBRepository: &MockDBRepository{}, plan: func() MetadataPlan {
		ready <- struct{}{}
		<-release
		return MetadataPlan{Parallelism: 1}
	}}
	for i := 0; i < contenders; i++ {
		go func() { _, err := loader.Start(context.Background(), 1, repo); results <- err }()
	}
	for i := 0; i < contenders; i++ {
		select {
		case <-ready:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("Starts did not all reach planner")
		}
	}
	close(release)
	accepted, alreadyStarted := 0, 0
	for i := 0; i < contenders; i++ {
		err := <-results
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrMetadataAlreadyStarted):
			alreadyStarted++
		default:
			t.Fatalf("unexpected Start error: %v", err)
		}
	}
	if accepted != 1 || alreadyStarted != contenders-1 {
		t.Fatalf("accepted=%d already-started=%d", accepted, alreadyStarted)
	}
	loader.Stop()
}

func TestMetadataLoaderCatalogJobSurvivesColumnFailure(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	columnErr := errors.New("columns unavailable")
	repository := catalogTestRepository()
	repository.MockDescribeDatabaseTable = func(context.Context) ([]*ColumnDesc, error) {
		return nil, columnErr
	}
	load, err := loader.Start(context.Background(), 1, repository)
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	snapshot := loader.Snapshot()
	if snapshot.Status[MetadataColumnsAll].State != MetadataFailed {
		t.Fatalf("columns state = %s, want failed", snapshot.Status[MetadataColumnsAll].State)
	}
	if !errors.Is(snapshot.Status[MetadataColumnsAll].Err, columnErr) {
		t.Fatalf("columns error = %v, want injected failure %v", snapshot.Status[MetadataColumnsAll].Err, columnErr)
	}
	if _, ok := snapshot.Cache.View("customer_view"); !ok {
		t.Fatalf("independent catalog job did not publish after column failure: state=%s err=%v cache=%+v", snapshot.Status[MetadataViews].State, snapshot.Status[MetadataViews].Err, snapshot.Cache.Catalog)
	}
	if snapshot.Status[MetadataViews].State != MetadataReady {
		t.Fatalf("views state = %s, want ready", snapshot.Status[MetadataViews].State)
	}
}
