package handler

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/language"
)

type interBaseChecker struct {
	db *sql.DB
}

func (c interBaseChecker) CheckStatement(ctx context.Context, query string) (*language.StatementError, error) {
	rejected, code, message, err := database.PrepareCheckInterBase(ctx, c.db, query)
	if err != nil || !rejected {
		return nil, err
	}
	statementError := language.ParseServerMessage(code, message)
	return &statementError, nil
}

type serverCheckLoggingChecker struct {
	checker language.StatementChecker
	runCtx  context.Context
	uri     string
	err     error
}

func (c *serverCheckLoggingChecker) CheckStatement(ctx context.Context, query string) (*language.StatementError, error) {
	statementError, err := c.checker.CheckStatement(ctx, query)
	if err != nil {
		c.err = err
		if c.runCtx.Err() == nil {
			log.Printf("sqls: server check skipped for %s: %v", c.uri, err)
		}
	}
	return statementError, err
}

type serverCheckState struct {
	mu       sync.Mutex
	timers   map[string]*time.Timer
	cancels  map[string]context.CancelFunc
	contexts map[string]context.Context
	runMu    sync.Mutex
	after    func(time.Duration, func()) *time.Timer
}

func newServerCheckState() *serverCheckState {
	return &serverCheckState{
		timers:   make(map[string]*time.Timer),
		cancels:  make(map[string]context.CancelFunc),
		contexts: make(map[string]context.Context),
		after:    time.AfterFunc,
	}
}

func (s *Server) scheduleServerCheck(uri string, delay time.Duration) {
	if s.serverCheck == nil || s.lifecycleCtx == nil || s.lifecycleCtx.Err() != nil {
		return
	}
	s.stateMu.RLock()
	enabled := s.effectiveConfigLocked().Diagnostics.ServerCheckEnabled()
	if !enabled {
		s.stateMu.RUnlock()
		s.cancelServerCheck(uri)
		return
	}

	state := s.serverCheck
	state.mu.Lock()
	if s.lifecycleCtx.Err() != nil {
		state.mu.Unlock()
		s.stateMu.RUnlock()
		return
	}
	if timer := state.timers[uri]; timer != nil {
		timer.Stop()
		delete(state.timers, uri)
	}
	if cancel := state.cancels[uri]; cancel != nil {
		cancel()
		delete(state.cancels, uri)
		delete(state.contexts, uri)
	}
	after := state.after
	if after == nil {
		after = time.AfterFunc
	}
	var timer *time.Timer
	ready := make(chan struct{})
	timer = after(delay, func() {
		<-ready
		state.mu.Lock()
		current := state.timers[uri] == timer
		if current {
			delete(state.timers, uri)
		}
		state.mu.Unlock()
		if current {
			s.runServerCheck(uri)
		}
	})
	if state.timers == nil {
		state.timers = make(map[string]*time.Timer)
	}
	state.timers[uri] = timer
	state.mu.Unlock()
	s.stateMu.RUnlock()
	close(ready)
}

func (s *Server) cancelServerCheck(uri string) {
	if s.serverCheck == nil {
		return
	}
	state := s.serverCheck
	state.mu.Lock()
	if timer := state.timers[uri]; timer != nil {
		timer.Stop()
		delete(state.timers, uri)
	}
	if cancel := state.cancels[uri]; cancel != nil {
		cancel()
		delete(state.cancels, uri)
		delete(state.contexts, uri)
	}
	state.mu.Unlock()
}

func (s *Server) scheduleAllServerChecks() {
	if s.serverCheck == nil || s.lifecycleCtx == nil || s.lifecycleCtx.Err() != nil {
		return
	}
	s.stateMu.RLock()
	diagnostics := s.effectiveConfigLocked().Diagnostics
	if !diagnostics.ServerCheckEnabled() {
		s.stateMu.RUnlock()
		s.cancelAllServerChecks()
		return
	}
	delay := diagnostics.ServerCheckDelay()
	uris := make([]string, 0, len(s.files))
	for uri := range s.files {
		uris = append(uris, uri)
	}
	s.stateMu.RUnlock()
	for _, uri := range uris {
		s.scheduleServerCheck(uri, delay)
	}
}

func (s *Server) cancelAllServerChecks() {
	if s.serverCheck == nil {
		return
	}
	state := s.serverCheck
	state.mu.Lock()
	for uri, timer := range state.timers {
		timer.Stop()
		delete(state.timers, uri)
	}
	for uri, cancel := range state.cancels {
		cancel()
		delete(state.cancels, uri)
		delete(state.contexts, uri)
	}
	state.mu.Unlock()
}

func (s *Server) stopAllServerChecks() {
	s.cancelAllServerChecks()
}

func (s *Server) statementCheckerLocked() language.StatementChecker {
	if s.dbConn == nil || s.dbConn.Driver != dialect.DatabaseDriverInterBase {
		return nil
	}
	if s.serverCheckerOverride != nil {
		return s.serverCheckerOverride()
	}
	if s.dbConn.Conn == nil {
		return nil
	}
	return interBaseChecker{db: s.dbConn.Conn}
}

func (s *Server) runServerCheck(uri string) {
	state := s.serverCheck
	if state == nil || s.lifecycleCtx == nil || s.lifecycleCtx.Err() != nil {
		return
	}
	snapshot, ok := s.diagnosticsSnapshot(uri)
	if !ok || !snapshot.dialectResolved || snapshot.variant.Driver != dialect.DatabaseDriverInterBase || !snapshot.serverCheckEnabled {
		return
	}
	s.stateMu.RLock()
	checker := s.statementCheckerLocked()
	s.stateMu.RUnlock()
	if checker == nil {
		return
	}

	ctx, cancel := context.WithCancel(s.lifecycleCtx)
	state.mu.Lock()
	if previous := state.cancels[uri]; previous != nil {
		previous()
	}
	state.cancels[uri] = cancel
	state.contexts[uri] = ctx
	state.mu.Unlock()
	state.runMu.Lock()
	defer func() {
		cancel()
		state.mu.Lock()
		if state.contexts[uri] == ctx {
			delete(state.contexts, uri)
			delete(state.cancels, uri)
		}
		state.mu.Unlock()
		state.runMu.Unlock()
	}()
	if ctx.Err() != nil || s.lifecycleCtx.Err() != nil || !s.diagnosticsSnapshotCurrent(snapshot) {
		return
	}

	request := languageRequest(editorSnapshotFromDiagnosticsSnapshot(snapshot))
	loggingChecker := &serverCheckLoggingChecker{checker: checker, runCtx: ctx, uri: uri}
	_, err := s.languageService.ServerDiagnostics(ctx, request, loggingChecker, language.ServerDiagnosticsOptions{
		Partition: serverCheckPartition(snapshot.metadataScope),
	})
	if err != nil {
		if ctx.Err() == nil && loggingChecker.err == nil {
			log.Printf("sqls: server check skipped for %s: %v", uri, err)
		}
		return
	}
	if loggingChecker.err != nil {
		return
	}
	if ctx.Err() == nil && s.diagnosticsSnapshotCurrent(snapshot) {
		s.queueDiagnosticDocument(uri)
	}
}
