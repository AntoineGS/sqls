package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestDiagnosticsCoalescesDirtyDocumentsAndAllOpenBit(t *testing.T) {
	s := &Server{
		lifecycleCtx:        context.Background(),
		diagnosticsWake:     make(chan struct{}, 1),
		diagnosticDocuments: make(map[string]struct{}),
	}

	for i := 0; i < 50; i++ {
		s.queueDiagnosticDocument("file:///same.sql")
	}
	s.queueDiagnosticDocument("file:///other.sql")
	documents, all := s.takeDiagnosticWork()
	if all || len(documents) != 2 {
		t.Fatalf("drained dirty docs = %v, all=%v; want two documents and no all bit", documents, all)
	}

	s.queueDiagnosticDocument("file:///same.sql")
	s.signalDiagnostics()
	s.queueAllDiagnostics()
	documents, all = s.takeDiagnosticWork()
	if !all || len(documents) != 0 {
		t.Fatalf("all-open work should subsume individual docs: %v, all=%v", documents, all)
	}

	s.queueDiagnosticDocument("file:///during-analysis.sql")
	documents, all = s.takeDiagnosticWork()
	if all || len(documents) != 1 || documents[0] != "file:///during-analysis.sql" {
		t.Fatalf("work arriving during a drain was lost: %v, all=%v", documents, all)
	}
}

func TestDiagnosticsLatestRevisionAndSingleAnalyzerAcrossBurst(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverMySQL)
	s := tx.server
	const uri = "file:///burst.sql"
	s.stateMu.Lock()
	s.notificationConn = tx.serverConn
	s.files[uri] = &File{Text: "initial", Version: 1, Revision: 1}
	s.stateMu.Unlock()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, peak atomic.Int32
	s.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		current := active.Add(1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return nil
	}

	s.queueDiagnosticDocument(uri)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not begin gated analysis")
	}

	for i := 0; i < 50; i++ {
		s.queueAllDiagnostics()
	}
	s.stateMu.Lock()
	s.files[uri] = &File{Text: "latest", Version: 9, Revision: 9}
	s.stateMu.Unlock()
	s.diagnosticWorkMu.Lock()
	if !s.diagnosticAllOpen || len(s.diagnosticDocuments) != 0 {
		s.diagnosticWorkMu.Unlock()
		t.Fatalf("pending metadata work was not coalesced: all=%v documents=%d", s.diagnosticAllOpen, len(s.diagnosticDocuments))
	}
	pendingDocuments := len(s.diagnosticDocuments)
	s.diagnosticWorkMu.Unlock()
	if pendingDocuments != 0 || len(s.diagnosticsWake) > 1 {
		t.Fatalf("unbounded pending work: documents=%d wake=%d", pendingDocuments, len(s.diagnosticsWake))
	}
	close(release)
	latest := tx.client.next(t, uri, func(notification diagnosticsNotification) bool {
		return notification.Version != nil && *notification.Version == 9
	})
	assertVersion(t, latest, 9)
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrent analyzers = %d, want 1", got)
	}
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && notification.Version != nil && *notification.Version == 1 {
			t.Fatalf("stale version 1 notification was published: %+v", notification)
		}
	}
}

func TestDiagnosticsStopCancelsRemainingAllOpenAnalysis(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverMySQL)
	s := tx.server
	first, second := "file:///stop-first.sql", "file:///stop-second.sql"
	s.stateMu.Lock()
	s.notificationConn = tx.serverConn
	s.files[first] = &File{Text: "first", Version: 1, Revision: 1}
	s.files[second] = &File{Text: "second", Version: 1, Revision: 2}
	s.stateMu.Unlock()

	entered := make(chan string, 2)
	release := make(chan struct{})
	s.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		entered <- snapshot.uri
		<-release
		return nil
	}
	s.queueAllDiagnostics()
	select {
	case uri := <-entered:
		if uri != first {
			t.Fatalf("first all-open URI = %s, want sorted first URI %s", uri, first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter first all-open analysis")
	}
	if err := s.Stop(); err != nil {
		t.Fatal("stop server:", err)
	}
	close(release)
	select {
	case <-s.diagnosticsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics worker did not stop")
	}
	select {
	case uri := <-entered:
		t.Fatalf("analysis started for %s after Stop", uri)
	default:
	}
}

func TestDiagnosticsCatalogReuseByCurrentCachePointer(t *testing.T) {
	s := NewServer()
	defer s.Stop()

	cache := &database.DBCache{}
	first := s.diagnosticCatalogFor(cache)
	if first == nil {
		t.Fatal("nil cache conversion")
	}
	// Metadata status revisions may reuse their data cache pointer.
	statusOnlyRevision := cache
	if got := s.diagnosticCatalogFor(statusOnlyRevision); got != first {
		t.Fatal("status-only revision rebuilt the derived catalog")
	}
	if got := s.diagnosticCatalogFor(&database.DBCache{}); got == first {
		t.Fatal("new cache pointer reused the prior derived catalog")
	}
}
