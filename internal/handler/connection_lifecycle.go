package handler

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"sync"

	"github.com/sqls-server/sqls/internal/database"
)

type connectionState string

const (
	connectionIdle       connectionState = "idle"
	connectionConnecting connectionState = "connecting"
	connectionReady      connectionState = "ready"
	connectionFailed     connectionState = "failed"
	connectionStopped    connectionState = "stopped"
)

type connectionIntent struct {
	ID              uint64
	Context         context.Context
	Config          *database.DBConfig
	ConnectionIndex int
	DatabaseName    string
	Reply           chan error
	explicit        bool
}

type connectionCoordinator struct {
	server       *Server
	wake         chan struct{}
	mu           sync.Mutex
	pending      *connectionIntent
	activeCancel context.CancelFunc
	stopped      bool
	nextID       uint64
	lastKey      string
	done         chan struct{}
}

func newConnectionCoordinator(s *Server) *connectionCoordinator {
	c := &connectionCoordinator{server: s, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go c.run()
	return c
}

func cloneConnectionConfig(cfg *database.DBConfig) *database.DBConfig {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	cp.Params = maps.Clone(cfg.Params)
	if cfg.SSHCfg != nil {
		ssh := *cfg.SSHCfg
		cp.SSHCfg = &ssh
	}
	if cfg.InterBase != nil {
		ib := *cfg.InterBase
		if cfg.InterBase.TLS != nil {
			tls := *cfg.InterBase.TLS
			ib.TLS = &tls
		}
		cp.InterBase = &ib
	}
	return &cp
}

func (c *connectionCoordinator) Request(ctx context.Context, cfg *database.DBConfig, index int, dbName string) <-chan error {
	return c.request(ctx, cfg, index, dbName, false)
}

func (c *connectionCoordinator) RequestExplicit(ctx context.Context, cfg *database.DBConfig, index int, dbName string) <-chan error {
	return c.request(ctx, cfg, index, dbName, true)
}

func (c *connectionCoordinator) request(ctx context.Context, cfg *database.DBConfig, index int, dbName string, explicit bool) <-chan error {
	reply := make(chan error, 1)
	if ctx == nil {
		ctx = c.server.lifecycleCtx
	}
	copyCfg := cloneConnectionConfig(cfg)
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		reply <- context.Canceled
		return reply
	}
	keyBytes, _ := json.Marshal(struct {
		C *database.DBConfig
		I int
		D string
	}{copyCfg, index, dbName})
	key := string(keyBytes)
	if c.pending != nil && (!reflect.DeepEqual(c.pending.Config, copyCfg) || explicit) {
		c.pending.Reply <- context.Canceled
	}
	if c.pending != nil && (!reflect.DeepEqual(c.pending.Config, copyCfg) || explicit) {
		c.pending = nil
	}
	if c.activeCancel != nil && !reflect.DeepEqual(c.server.activeIntentConfig(), copyCfg) {
		c.activeCancel()
	}
	// Avoid retrying an unchanged failed/active notification. Explicit requests
	// are refreshes and intentionally bypass this identity check.
	if !explicit && key == c.lastKey && c.pending == nil {
		c.mu.Unlock()
		reply <- nil
		return reply
	}
	c.nextID++
	c.pending = &connectionIntent{ID: c.nextID, Context: ctx, Config: copyCfg, ConnectionIndex: index, DatabaseName: dbName, Reply: reply, explicit: explicit}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return reply
}

func (c *connectionCoordinator) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	if c.pending != nil {
		c.pending.Reply <- context.Canceled
		c.pending = nil
	}
	if c.activeCancel != nil {
		c.activeCancel()
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *connectionCoordinator) run() {
	defer close(c.done)
	for {
		select {
		case <-c.server.lifecycleCtx.Done():
			c.Stop()
			return
		case <-c.wake:
		}
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		intent := c.pending
		c.pending = nil
		c.mu.Unlock()
		if intent == nil {
			continue
		}
		if err := intent.Context.Err(); err != nil {
			intent.Reply <- err
			continue
		}
		ctx, cancel := context.WithCancel(c.server.lifecycleCtx)
		stopCancel := context.AfterFunc(intent.Context, cancel)
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			stopCancel()
			cancel()
			intent.Reply <- context.Canceled
			return
		}
		c.activeCancel = cancel
		c.mu.Unlock()
		err := c.server.attachIntent(ctx, intent)
		stopCancel()
		cancel()
		c.mu.Lock()
		c.activeCancel = nil
		c.lastKey = intentKey(intent)
		c.mu.Unlock()
		intent.Reply <- err
	}
}

func intentKey(i *connectionIntent) string {
	b, _ := json.Marshal(struct {
		C *database.DBConfig
		I int
		D string
	}{i.Config, i.ConnectionIndex, i.DatabaseName})
	return string(b)
}

var errCoordinatorStopped = errors.New("connection coordinator stopped")
