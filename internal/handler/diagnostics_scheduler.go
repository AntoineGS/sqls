package handler

import (
	"sort"
)

// queueDiagnosticDocument marks one URI dirty before waking the single
// diagnostics consumer. The channel carries no document state, only a wakeup.
func (s *Server) queueDiagnosticDocument(uri string) {
	s.diagnosticWorkMu.Lock()
	if !s.diagnosticAllOpen {
		s.diagnosticDocuments[uri] = struct{}{}
	}
	s.diagnosticWorkMu.Unlock()
	s.signalDiagnostics()
}

// queueAllDiagnostics coalesces metadata/cache changes into one all-open bit.
func (s *Server) queueAllDiagnostics() {
	s.diagnosticWorkMu.Lock()
	s.diagnosticAllOpen = true
	s.diagnosticDocuments = make(map[string]struct{})
	s.diagnosticWorkMu.Unlock()
	s.signalDiagnostics()
}

// takeDiagnosticWork atomically detaches the current dirty set. Work queued
// while it is being processed remains set for the next wake/drain.
func (s *Server) takeDiagnosticWork() ([]string, bool) {
	s.diagnosticWorkMu.Lock()
	all := s.diagnosticAllOpen
	documents := make([]string, 0, len(s.diagnosticDocuments))
	if !all {
		for uri := range s.diagnosticDocuments {
			documents = append(documents, uri)
		}
	}
	s.diagnosticAllOpen = false
	s.diagnosticDocuments = make(map[string]struct{})
	s.diagnosticWorkMu.Unlock()
	sort.Strings(documents)
	return documents, all
}

func (s *Server) runDiagnosticSignals() {
	defer close(s.diagnosticsDone)
	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-s.diagnosticsWake:
		}
		for {
			documents, all := s.takeDiagnosticWork()
			if all {
				s.republishOpenDiagnostics(s.lifecycleCtx)
			} else if len(documents) > 0 {
				s.stateMu.RLock()
				conn := s.notificationConn
				s.stateMu.RUnlock()
				for _, uri := range documents {
					if s.lifecycleCtx.Err() != nil {
						return
					}
					s.publishDocumentDiagnostics(s.lifecycleCtx, conn, uri)
				}
			}
			// A wake can be consumed before a concurrent enqueue signals. Loop
			// over detached work; if empty, return to select for a fresh wake.
			s.diagnosticWorkMu.Lock()
			pending := s.diagnosticAllOpen || len(s.diagnosticDocuments) != 0
			s.diagnosticWorkMu.Unlock()
			if !pending {
				break
			}
		}
	}
}
