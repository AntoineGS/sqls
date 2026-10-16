package handler

import (
	"context"
	"fmt"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

// editorSnapshot is the immutable set of editor, connection, and metadata
// inputs used by one language-server operation. Cache and Metadata belong to
// the same connection generation as Variant and Repository.
//
// Generation and MetadataGeneration/MetadataIncarnation are DIFFERENT
// identities (see handler.go's metaBoundGeneration doc comment):
// Generation is the request-binding guard for query execution and this
// snapshot's own staleness (did the connection get replaced), while
// MetadataIncarnation identifies the shared store instance and
// MetadataGeneration the store's own revision count (does it get bumped by a
// Refresh of the SAME connection). A staleness check that only compares
// Generation cannot tell "the connection changed" apart from "the metadata
// I'm holding was refreshed under the same connection" — callers that care
// about the latter compare MetadataIncarnation/MetadataGeneration instead.
//
// MetadataLease is the same lease Metadata/MetadataIncarnation/
// MetadataGeneration were captured from (nil under the same conditions they
// are zero). Language-service detail requests use this lease so they share
// retained and in-flight loads with other consumers of the same store.
type editorSnapshot struct {
	URI, Text           string
	Version             int
	Revision            uint64
	CacheEpoch          uint64
	Generation          int
	Variant             dialect.DriverVariant
	DialectResolved     bool
	Cache               *database.DBCache
	Metadata            *database.MetadataSnapshot
	MetadataIncarnation uint64
	MetadataGeneration  uint64
	MetadataKey         metadata.Key
	MetadataLease       *metadata.Lease
	Repository          database.DBRepository
	LowercaseKeywords   bool
	DiagnosticPolicy    language.DiagnosticPolicy
	PolicyRevision      uint64
	Attaching           bool
	ConnectionReady     bool
	SourceContext       snapshotContext
}

func (s *Server) captureEditorSnapshot(uri string) (editorSnapshot, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.captureEditorSnapshotLocked(uri)
}

// captureEditorSnapshotLocked is the stateMu-held form used by publication
// fences. It must not acquire stateMu itself.
func (s *Server) captureEditorSnapshotLocked(uri string) (editorSnapshot, error) {
	file := s.files[uri]
	if file == nil {
		return editorSnapshot{}, fmt.Errorf("document not found: %s", uri)
	}

	configSnapshot := s.effectiveConfigLocked()
	cfg := cloneConnectionConfig(s.curDBCfg)
	if cfg == nil {
		cfg = selectedConfig(configSnapshot, s.initOptionDBConfig, s.curConnectionIndex)
	}

	snapshot := editorSnapshot{
		URI: uri, Text: file.Text, Version: file.Version, Revision: file.Revision,
		CacheEpoch: s.languageService.CacheEpoch(),
		Generation: s.connGeneration, Cache: &database.DBCache{},
		LowercaseKeywords: configSnapshot.LowercaseKeywords,
		DiagnosticPolicy:  language.DiagnosticPolicy{Rules: cloneDiagnosticRules(configSnapshot.Diagnostics.Rules)},
		PolicyRevision:    s.policyRevision,
		Attaching:         s.connectionState == connectionConnecting,
		ConnectionReady:   s.connectionState == connectionReady,
	}
	if cfg != nil {
		snapshot.SourceContext = snapshotContextForConfig(snapshot.Generation, cfg)
		snapshot.Variant.Driver = cfg.Driver
		if cfg.Driver == dialect.DatabaseDriverInterBase {
			if cfg.Dialect == 1 {
				snapshot.Variant.Variant = dialect.SQLVariantInterBase1
				snapshot.DialectResolved = true
			} else if cfg.Dialect == 3 {
				snapshot.Variant.Variant = dialect.SQLVariantInterBase3
				snapshot.DialectResolved = true
			} else {
				// Auto detection defaults to Dialect 3 for parser behavior until
				// the attachment reports its server-side dialect.
				snapshot.Variant.Variant = dialect.SQLVariantInterBase3
			}
		} else {
			snapshot.DialectResolved = true
		}
	}

	if s.metaLease != nil && s.connGeneration == s.metaBoundGeneration {
		meta := s.metaLease.Snapshot()
		if meta != nil {
			snapshot.Metadata = meta
			snapshot.MetadataIncarnation = s.metaLease.Incarnation()
			snapshot.MetadataGeneration = meta.Generation
			snapshot.MetadataKey = s.metaLease.Key()
			snapshot.MetadataLease = s.metaLease
			if meta.Cache != nil {
				snapshot.Cache = meta.Cache
			}
		}
	}

	if s.dbConn != nil {
		resolved := s.dbConn.DriverVariant()
		if resolved.Driver != "" {
			snapshot.Variant.Driver = resolved.Driver
		}
		snapshot.Variant.Variant = resolved.Variant
		snapshot.DialectResolved = true
		if s.connectionState == connectionReady && cfg != nil {
			// Both inputs were captured above while stateMu was held.
			snapshot.Repository, _ = database.CreateRepositoryFromConnection(cfg.Driver, s.dbConn)
		}
	}
	return snapshot, nil
}

func cloneDiagnosticRules(rules map[string]string) map[string]string {
	if len(rules) == 0 {
		return nil
	}
	clone := make(map[string]string, len(rules))
	for code, level := range rules {
		clone[code] = level
	}
	return clone
}

// publishDefinitionCandidate writes privately, then validates and publishes
// the candidate at one stateMu linearization point. A stale attempt can only
// remove its own unique candidate directory.
func (s *Server) publishDefinitionCandidate(ctx context.Context, captured editorSnapshot, stamp language.Stamp, kind, name, content string) (string, bool, error) {
	candidate, err := s.snapshots.writeCandidate(captured.SourceContext, kind, name, content)
	if err != nil {
		return "", false, err
	}
	if afterWrite := s.snapshots.afterCandidateWrite; afterWrite != nil {
		afterWrite()
	}
	s.stateMu.RLock()
	current, captureErr := s.captureEditorSnapshotLocked(captured.URI)
	valid := ctx.Err() == nil && captureErr == nil &&
		current.Revision == captured.Revision && current.Text == captured.Text &&
		current.Version == captured.Version && current.Variant == captured.Variant &&
		current.DialectResolved == captured.DialectResolved &&
		current.Generation == captured.Generation &&
		current.SourceContext == captured.SourceContext &&
		current.MetadataIncarnation == captured.MetadataIncarnation &&
		current.MetadataGeneration == captured.MetadataGeneration &&
		metadataRevision(current) == metadataRevision(captured) &&
		metadataKeyEqual(current, captured) &&
		language.Current(stamp, languageRequest(current).Document, languageRequest(current).Context)
	s.stateMu.RUnlock()
	if !valid {
		candidate.remove()
		return "", false, nil
	}
	return candidate.path, true, nil
}

func metadataKeyEqual(a, b editorSnapshot) bool {
	return a.MetadataKey == b.MetadataKey && (a.MetadataLease == nil) == (b.MetadataLease == nil)
}

func metadataRevision(snapshot editorSnapshot) uint64 {
	if snapshot.Metadata == nil {
		return 0
	}
	return snapshot.Metadata.Revision
}

func (s *Server) snapshotGenerationCurrent(generation int) bool {
	s.stateMu.RLock()
	current := s.connGeneration == generation && s.connectionState != connectionStopped
	s.stateMu.RUnlock()
	return current
}

// writeDefinitionSnapshot writes to an attempt-private path without holding
// stateMu, then uses a short read lock as the location-publication
// linearization point. Stale candidates are removed only from their own
// directory, outside the lifecycle lock.
func (s *Server) writeDefinitionSnapshot(sc snapshotContext, kind, name, content string) (string, bool, error) {
	candidate, err := s.snapshots.writeCandidate(sc, kind, name, content)
	if err != nil {
		return "", false, err
	}
	s.stateMu.RLock()
	current := s.connGeneration == sc.generation && s.connectionState != connectionStopped
	s.stateMu.RUnlock()
	if !current {
		candidate.remove()
		return "", false, nil
	}
	return candidate.path, true, nil
}

func snapshotContextForConfig(generation int, cfg *database.DBConfig) snapshotContext {
	sc := snapshotContext{generation: generation, label: "(unnamed)"}
	if cfg == nil {
		return sc
	}
	if cfg.Alias != "" {
		sc.label = cfg.Alias
	}
	sc.identity = fmt.Sprintf("%s|%s|%s|%d|%s|%s", cfg.Driver, cfg.DataSourceName, cfg.Host, cfg.Port, cfg.Path, cfg.DBName)
	return sc
}

func (s *Server) effectiveConfigLocked() *config.Config {
	switch {
	case validConfig(s.SpecificFileCfg):
		return s.SpecificFileCfg
	case validConfig(s.WSCfg):
		return s.WSCfg
	case validConfig(s.DefaultFileCfg):
		return s.DefaultFileCfg
	default:
		return config.NewConfig()
	}
}

func selectedConfig(effective *config.Config, init *database.DBConfig, index int) *database.DBConfig {
	if init != nil {
		return cloneConnectionConfig(init)
	}
	if effective == nil || index < 0 || index >= len(effective.Connections) {
		index = 0
	}
	if effective == nil || len(effective.Connections) == 0 {
		return nil
	}
	return cloneConnectionConfig(effective.Connections[index])
}
