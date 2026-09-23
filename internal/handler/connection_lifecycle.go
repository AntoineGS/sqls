package handler

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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
	activeKey    string
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
	intent := &connectionIntent{Config: copyCfg, ConnectionIndex: index, DatabaseName: dbName, Reply: reply, explicit: explicit}
	key := intentKey(intent)
	var replaced chan error
	if !explicit {
		if c.pending != nil && intentKey(c.pending) == key {
			c.mu.Unlock()
			reply <- nil
			return reply
		}
		if c.pending == nil && (c.activeKey == key || c.lastKey == key) {
			c.mu.Unlock()
			reply <- nil
			return reply
		}
	}
	if c.pending != nil {
		replaced = c.pending.Reply
		c.pending = nil
	}
	if c.activeCancel != nil && c.activeKey != key {
		c.activeCancel()
	}
	c.nextID++
	intent.ID, intent.Context = c.nextID, ctx
	c.pending = intent
	c.mu.Unlock()
	if replaced != nil {
		replaced <- context.Canceled
	}
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
	var pending chan error
	if c.pending != nil {
		pending = c.pending.Reply
		c.pending = nil
	}
	if c.activeCancel != nil {
		c.activeCancel()
	}
	c.mu.Unlock()
	if pending != nil {
		pending <- context.Canceled
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func awaitConnectionIntent(ctx context.Context, reply <-chan error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
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
		if intent != nil {
			c.activeKey = intentKey(intent)
		}
		c.mu.Unlock()
		if intent == nil {
			continue
		}
		if err := intent.Context.Err(); err != nil {
			c.mu.Lock()
			if c.activeKey == intentKey(intent) {
				c.activeKey = ""
			}
			c.mu.Unlock()
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
		c.activeKey = ""
		if !errors.Is(err, context.Canceled) {
			c.lastKey = intentKey(intent)
		}
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
