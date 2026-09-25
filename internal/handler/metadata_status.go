package handler

import (
	"context"
	"time"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type MetadataStatusResult = lsp.MetadataStatusResult

// Keep this order aligned with the metadata plan vocabulary; status output must
// not depend on iteration order of the loader's status map.
var metadataKindOrder = []database.MetadataKind{
	database.MetadataSchemas, database.MetadataRelations, database.MetadataColumnsCurrent,
	database.MetadataColumnsAll, database.MetadataPrimaryKeys, database.MetadataForeignKeys,
	database.MetadataViews, database.MetadataProcedures, database.MetadataGenerators,
	database.MetadataDomains, database.MetadataFunctions, database.MetadataIndexes, database.MetadataTriggers,
}

func (s *Server) metadataStatus() lsp.MetadataStatusResult {
	s.stateMu.RLock()
	state, connGeneration := s.connectionState, s.connGeneration
	lease, bound := s.metaLease, s.metaBoundGeneration
	s.stateMu.RUnlock()
	result := lsp.MetadataStatusResult{ConnectionGeneration: uint64(connGeneration), ConnectionState: string(state), Categories: []lsp.MetadataCategoryStatus{}}
	if state == connectionFailed {
		result.ConnectionErrorCode, result.Settled, result.Degraded = "connection_failed", true, true
		return result
	}
	if state != connectionReady || lease == nil || connGeneration != bound {
		// Metadata has not yet been bound to the currently active connection
		// (attachIntent's Acquire/Refresh step has not completed, or a newer
		// intent has already superseded this one) — the same "not caught up
		// yet" shape the function has always produced for that window.
		return result
	}
	snapshot := lease.Snapshot()
	result.Generation = snapshot.Generation
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
		var duration time.Duration
		if !status.StartedAt.IsZero() {
			end := status.FinishedAt
			if end.IsZero() {
				end = time.Now()
			}
			duration = end.Sub(status.StartedAt)
			if duration < 0 {
				duration = 0
			}
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
