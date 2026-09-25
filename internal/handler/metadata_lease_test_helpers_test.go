package handler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

var testMetadataKeySeq atomic.Uint64

// bindMetadataRepoForTest acquires a fresh, uniquely-keyed lease wrapping repo
// via the same generic provider adapter production non-InterBase connections
// use (genericMetadataProvider, from metadata_context.go), binds it onto s as
// the active lease for generation, and returns it. It mimics bindMetadata's
// Acquire branch for tests that need direct control over the repository/plan
// without driving the full connection lifecycle — the direct replacement for
// the old "s.metadata.Reset(generation); s.metadata.Start(ctx, generation,
// repo)" pattern.
func bindMetadataRepoForTest(t testing.TB, s *Server, generation int, repo database.DBRepository) *metadata.Lease {
	t.Helper()
	seq := testMetadataKeySeq.Add(1)
	key := metadata.Key{Engine: "test", URL: fmt.Sprintf("test://%d", seq)}
	scope := metadata.Scope{Principal: "test"}
	factory := func(context.Context) (metadata.Provider, error) {
		return &genericMetadataProvider{repo: repo}, nil
	}
	lease, err := s.registry.Acquire(context.Background(), key, scope, factory)
	if err != nil {
		t.Fatalf("acquire test metadata lease: %v", err)
	}
	sub := lease.Subscribe()
	s.stateMu.Lock()
	oldLease, oldSub := s.metaLease, s.metaSub
	s.metaLease, s.metaSub = lease, sub
	s.metaKey, s.metaScope = key, scope
	s.metaBoundGeneration = generation
	s.stateMu.Unlock()
	if oldSub != nil {
		oldSub.Close()
	}
	if oldLease != nil {
		oldLease.Release()
	}
	go s.watchMetadataEvents(sub)
	return lease
}

// currentLeaseForTest reads the currently bound lease, or nil if none is
// bound. Tests that drive metadata binding through the real RPC/attachIntent
// path (rather than bindMetadataRepoForTest) use this to reach it.
func currentLeaseForTest(s *Server) *metadata.Lease {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.metaLease
}

// waitCurrentMetadataSettledForTest waits for the currently bound lease to
// settle. The direct replacement for the old "s.metadata.Wait(ctx)" pattern
// for tests that drive metadata binding through the real RPC/attachIntent
// path.
func waitCurrentMetadataSettledForTest(t testing.TB, ctx context.Context, s *Server) {
	t.Helper()
	lease := currentLeaseForTest(s)
	if lease == nil {
		t.Fatal("no metadata lease bound")
	}
	waitMetadataSettledForTest(t, ctx, lease)
}

// waitMetadataSettledForTest waits for lease's published snapshot to settle
// (every category reaches a terminal state), the direct replacement for the
// old "load, err := s.metadata.Start(...); <-load.Done" / "s.metadata.Wait(ctx)"
// synchronous-settlement patterns.
func waitMetadataSettledForTest(t testing.TB, ctx context.Context, lease *metadata.Lease) {
	t.Helper()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("wait for metadata readiness: %v", err)
	}
}

// currentLeaseCacheForTest reads the currently bound lease's published Cache,
// or nil if no lease is bound. The direct replacement for the old
// "s.metadata.Cache()" read.
func currentLeaseCacheForTest(s *Server) *database.DBCache {
	s.stateMu.RLock()
	lease := s.metaLease
	s.stateMu.RUnlock()
	if lease == nil {
		return nil
	}
	snapshot := lease.Snapshot()
	if snapshot == nil {
		return nil
	}
	return snapshot.Cache
}

// loadMetadataForTest installs a fresh metadata lease for repo just like a
// successful attachment transition, then waits for deterministic settlement
// rather than sleeping.
func loadMetadataForTest(t *testing.T, s *Server, repo database.DBRepository) {
	t.Helper()
	s.diagnosticsPublishMu.Lock()
	s.stateMu.Lock()
	s.connGeneration++
	generation := s.connGeneration
	s.stateMu.Unlock()
	s.diagnosticsPublishMu.Unlock()
	lease := bindMetadataRepoForTest(t, s, generation, repo)
	waitMetadataSettledForTest(t, context.Background(), lease)
	s.republishOpenDiagnostics(context.Background())
}
