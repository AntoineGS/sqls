package handler

import (
	"context"
	"sync"
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
	s := NewServer()
	defer s.Stop()
	const uri = "file:///burst.sql"
	s.stateMu.Lock()
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverMySQL}
	s.files[uri] = &File{Text: "initial", Version: 1, Revision: 1}
	s.stateMu.Unlock()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, peak atomic.Int32
	var versionsMu sync.Mutex
	var versions []int
	s.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		current := active.Add(1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		versionsMu.Lock()
		versions = append(versions, snapshot.version)
		versionsMu.Unlock()
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
		s.signalDiagnostics()
	}
	s.stateMu.Lock()
	s.files[uri] = &File{Text: "latest", Version: 9, Revision: 9}
	s.stateMu.Unlock()
	s.queueDiagnosticDocument(uri)
	close(release)

	deadline := time.After(5 * time.Second)
	for {
		versionsMu.Lock()
		finishedLatest := len(versions) >= 2 && versions[len(versions)-1] == 9
		versionsMu.Unlock()
		if finishedLatest {
			break
		}
		select {
		case <-entered:
		case <-deadline:
			t.Fatal("worker did not analyze the latest revision")
		}
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrent analyzers = %d, want 1", got)
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
