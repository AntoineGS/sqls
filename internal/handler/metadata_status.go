package handler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type MetadataStatusResult = lsp.MetadataStatusResult

var metadataKindOrder = []database.MetadataKind{
	database.MetadataSchemas, database.MetadataRelations, database.MetadataColumnsCurrent,
	database.MetadataColumnsAll, database.MetadataPrimaryKeys, database.MetadataForeignKeys,
	database.MetadataViews, database.MetadataProcedures, database.MetadataGenerators,
	database.MetadataDomains, database.MetadataFunctions, database.MetadataIndexes, database.MetadataTriggers,
}

func (s *Server) metadataStatus() lsp.MetadataStatusResult {
	s.stateMu.RLock()
	state, generation := s.connectionState, uint64(s.connGeneration)
	s.stateMu.RUnlock()
	result := lsp.MetadataStatusResult{Generation: generation, ConnectionState: string(state), Categories: []lsp.MetadataCategoryStatus{}}
	if state == connectionFailed {
		result.ConnectionErrorCode, result.Settled, result.Degraded = "connection_failed", true, true
		return result
	}
	if state != connectionReady {
		return result
	}
	snapshot := s.metadata.Snapshot()
	if snapshot.Generation != generation {
		return result
	}
	result.Revision = snapshot.Revision
	if snapshot.StartFailed {
		result.Settled, result.Degraded, result.MetadataErrorCode = true, true, "metadata_load_failed"
		return result
	}
	for _, kind := range metadataKindOrder {
		status, ok := snapshot.Status[kind]
		if !ok {
			continue
		}
		started := status.StartedAt
		if started.IsZero() {
			started = status.QueuedAt
		}
		var duration time.Duration
		if !started.IsZero() {
			end := status.FinishedAt
			if end.IsZero() {
				end = time.Now()
			}
			duration = end.Sub(started)
		}
		category := lsp.MetadataCategoryStatus{Kind: string(kind), State: string(status.State), Count: status.Count, DurationMS: duration.Milliseconds()}
		switch status.State {
		case database.MetadataFailed:
			category.ErrorCode = "metadata_load_failed"
		case database.MetadataBlocked:
			category.ErrorCode = "dependency_failed"
		case database.MetadataCancelled:
			category.ErrorCode = "cancelled"
		}
		result.Categories = append(result.Categories, category)
	}
	// Preserve the declared order even if a future driver introduces map-backed statuses.
	sort.SliceStable(result.Categories, func(i, j int) bool {
		return metadataKindIndex(result.Categories[i].Kind) < metadataKindIndex(result.Categories[j].Kind)
	})
	result.Settled = snapshot.Settled()
	if snapshot.Started && len(snapshot.Status) == 0 {
		result.Settled = true
	}
	if result.Settled {
		for _, category := range result.Categories {
			if category.State == string(database.MetadataFailed) || category.State == string(database.MetadataBlocked) || category.State == string(database.MetadataCancelled) {
				result.Degraded = true
			}
		}
	}
	return result
}

func (s *Server) showMetadataStatus(context.Context, lsp.ExecuteCommandParams) (interface{}, error) {
	return s.metadataStatus(), nil
}

func (s *Server) signalMetadata() {
	select {
	case s.metadataWake <- struct{}{}:
	default:
	}
}

func metadataKindIndex(kind string) int {
	for index, candidate := range metadataKindOrder {
		if string(candidate) == kind {
			return index
		}
	}
	return len(metadataKindOrder)
}

func (s *Server) runMetadataReporter() {
	defer close(s.metadataReporterDone)
	var generation uint64
	var token string
	progressEnabled := false
	fallbackDone := false
	createAttempted := false
	var lastReport time.Time
	for {
		select {
		case <-s.lifecycleCtx.Done():
			if token != "" {
				s.sendMetadataProgress(token, map[string]interface{}{"kind": "end", "message": "Metadata loading cancelled"})
			}
			return
		case <-s.metadataWake:
		}
		status := s.metadataStatus()
		if generation != status.Generation {
			if token != "" {
				s.sendMetadataProgress(token, map[string]interface{}{"kind": "end", "message": "Metadata loading cancelled"})
			}
			generation, token, progressEnabled, fallbackDone, createAttempted = status.Generation, "", false, false, false
		}
		if generation == 0 || !s.isInitialized() || status.ConnectionState == string(connectionIdle) {
			continue
		}
		if token == "" && !fallbackDone && !createAttempted {
			token = fmt.Sprintf("sqls-metadata-%d", generation)
			if s.supportsWorkDoneProgress() {
				createAttempted = true
				ctx, cancel := context.WithTimeout(s.lifecycleCtx, 500*time.Millisecond)
				var conn *jsonrpc2.Conn
				s.stateMu.RLock()
				conn = s.notificationConn
				s.stateMu.RUnlock()
				if conn != nil && conn.Call(ctx, "window/workDoneProgress/create", map[string]interface{}{"token": token}, nil) == nil {
					progressEnabled = true
				} else {
					token = ""
				}
				cancel()
			}
			if progressEnabled {
				s.sendMetadataProgress(token, map[string]interface{}{"kind": "begin", "title": "Loading database metadata", "cancellable": false})
			}
		}
		if status.Settled {
			if progressEnabled {
				message := "Metadata loading complete"
				if status.ConnectionErrorCode != "" {
					message = "Database connection failed"
				} else if status.Degraded {
					message = "Metadata loading completed with failures"
				}
				s.sendMetadataProgress(token, map[string]interface{}{"kind": "end", "message": message})
			} else if !fallbackDone {
				message := "Metadata loading complete"
				if status.ConnectionErrorCode != "" {
					message = "Database connection failed"
				} else if status.Degraded {
					message = "Metadata loading completed with failures"
				}
				s.sendMetadataLog(message, status.Degraded)
			}
			token, fallbackDone = "", true
			continue
		}
		if progressEnabled && time.Since(lastReport) >= 100*time.Millisecond {
			completed, total := 0, len(status.Categories)
			for _, category := range status.Categories {
				if category.State != string(database.MetadataPending) && category.State != string(database.MetadataLoading) {
					completed++
				}
			}
			s.sendMetadataProgress(token, map[string]interface{}{"kind": "report", "message": fmt.Sprintf("Metadata %d/%d categories", completed, total), "percentage": percentage(completed, total)})
			lastReport = time.Now()
		}
	}
}

func percentage(done, total int) int {
	if total == 0 {
		return 0
	}
	return done * 100 / total
}
func (s *Server) isInitialized() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.initialized
}
func (s *Server) supportsWorkDoneProgress() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.workDoneProgress
}
func (s *Server) sendMetadataProgress(token string, value interface{}) {
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, 200*time.Millisecond)
	defer cancel()
	s.stateMu.RLock()
	conn := s.notificationConn
	s.stateMu.RUnlock()
	if conn != nil {
		_ = conn.Notify(ctx, "$/progress", map[string]interface{}{"token": token, "value": value})
	}
}
func (s *Server) sendMetadataLog(message string, degraded bool) {
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, 200*time.Millisecond)
	defer cancel()
	s.stateMu.RLock()
	conn := s.notificationConn
	s.stateMu.RUnlock()
	if conn == nil {
		return
	}
	_ = conn.Notify(ctx, "window/logMessage", map[string]interface{}{"type": 3, "message": message})
	if degraded {
		_ = conn.Notify(ctx, "window/logMessage", map[string]interface{}{"type": 2, "message": "Database metadata is incomplete; run sqls.showMetadataStatus for details."})
	}
}
