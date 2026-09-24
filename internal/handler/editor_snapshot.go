package handler

import (
	"fmt"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// editorSnapshot is the immutable set of editor, connection, and metadata
// inputs used by one language-server operation. Cache and Metadata belong to
// the same connection generation as Variant and Repository.
type editorSnapshot struct {
	URI, Text         string
	Version           int
	Revision          uint64
	Generation        int
	Variant           dialect.DriverVariant
	DialectResolved   bool
	Cache             *database.DBCache
	Metadata          *database.MetadataSnapshot
	Repository        database.DBRepository
	LowercaseKeywords bool
	DiagnosticOptions sqlsymbol.DiagnosticOptions
	PolicyRevision    uint64
	Attaching         bool
	ConnectionReady   bool
	SourceContext     snapshotContext
}

func (s *Server) captureEditorSnapshot(uri string) (editorSnapshot, error) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

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
		Generation: s.connGeneration, Cache: &database.DBCache{},
		LowercaseKeywords: configSnapshot.LowercaseKeywords,
		DiagnosticOptions: configSnapshot.Diagnostics.Options(),
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

	metadata := s.metadata.Snapshot()
	if metadata != nil && metadata.Generation == uint64(snapshot.Generation) {
		snapshot.Metadata = metadata
		if metadata.Cache != nil {
			snapshot.Cache = metadata.Cache
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
