package database

import (
	"errors"
	"testing"
)

func TestMetadataReadyDistinguishesEmptyFromUnknown(t *testing.T) {
	cache := &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{}},
		Metadata: map[MetadataKind]MetadataState{MetadataViews: MetadataLoading}}
	if cache.MetadataReady(MetadataViews) {
		t.Fatal("loading is not complete")
	}
	cache.Metadata[MetadataViews] = MetadataReady
	if !cache.MetadataReady(MetadataViews) {
		t.Fatal("empty success is complete")
	}
	if cache.MetadataReady(MetadataIndexes) {
		t.Fatal("absent state is unknown")
	}
}

func TestMetadataReadinessAndSnapshotSettlement(t *testing.T) {
	if (*DBCache)(nil).MetadataReady(MetadataViews) || (*DBCache)(nil).ColumnsReady() {
		t.Fatal("nil cache must not be ready")
	}
	cache := &DBCache{Metadata: map[MetadataKind]MetadataState{
		MetadataColumnsCurrent: MetadataLoading,
		MetadataColumnsAll:     MetadataReady,
	}}
	if !cache.ColumnsReady() {
		t.Fatal("all-columns readiness should satisfy column readiness")
	}
	if cache.MetadataReady() {
		t.Fatal("readiness with no required kinds must be false")
	}

	for _, snapshot := range []*MetadataSnapshot{nil, {}, {Status: map[MetadataKind]MetadataStatus{}}} {
		if snapshot.Settled() || snapshot.Degraded() {
			t.Fatalf("empty snapshot should be neither settled nor degraded: %#v", snapshot)
		}
	}
	for _, tc := range []struct {
		state    MetadataState
		settled  bool
		degraded bool
	}{
		{MetadataPending, false, false},
		{MetadataLoading, false, false},
		{MetadataReady, true, false},
		{MetadataFailed, true, true},
		{MetadataBlocked, true, true},
		{MetadataCancelled, true, false},
		{MetadataUnsupported, true, false},
	} {
		snapshot := &MetadataSnapshot{Status: map[MetadataKind]MetadataStatus{MetadataViews: {State: tc.state}}}
		if got := snapshot.Settled(); got != tc.settled {
			t.Errorf("Settled() for %q = %v, want %v", tc.state, got, tc.settled)
		}
		if got := snapshot.Degraded(); got != tc.degraded {
			t.Errorf("Degraded() for %q = %v, want %v", tc.state, got, tc.degraded)
		}
	}
	failed := &MetadataSnapshot{Status: map[MetadataKind]MetadataStatus{
		MetadataViews:   {State: MetadataFailed, Err: errors.New("internal")},
		MetadataIndexes: {State: MetadataCancelled},
	}}
	if !failed.Degraded() || !failed.Settled() {
		t.Fatal("failed/terminal mixture should be settled and degraded")
	}
}
