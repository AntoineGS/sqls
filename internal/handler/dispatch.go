package handler

import (
	"context"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
)

// cancelParams is the payload of the LSP $/cancelRequest notification.
// jsonrpc2.ID unmarshals both the numeric and the string form.
type cancelParams struct {
	ID jsonrpc2.ID `json:"id"`
}

// cancelEntry is one in-flight cancellable request. requested stays true after
// a cancellation even when the statement completed anyway, so the caller can
// tell the user that the result it is about to read is the real result.
type cancelEntry struct {
	cancel context.CancelFunc

	mu        sync.Mutex
	requested bool
}

func (e *cancelEntry) cancelRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requested
}

func (e *cancelEntry) request() {
	e.mu.Lock()
	e.requested = true
	e.mu.Unlock()
	e.cancel()
}

type cancelRegistry struct {
	mu      sync.Mutex
	entries map[jsonrpc2.ID]*cancelEntry
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{entries: map[jsonrpc2.ID]*cancelEntry{}}
}

func (r *cancelRegistry) register(id jsonrpc2.ID, cancel context.CancelFunc) *cancelEntry {
	entry := &cancelEntry{cancel: cancel}
	r.mu.Lock()
	r.entries[id] = entry
	r.mu.Unlock()
	return entry
}

func (r *cancelRegistry) unregister(id jsonrpc2.ID) {
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
}

// cancel is a no-op for an id that is not in flight: a cancellation that races
// the response is normal and must not error.
func (r *cancelRegistry) cancel(id jsonrpc2.ID) {
	r.mu.Lock()
	entry := r.entries[id]
	r.mu.Unlock()
	if entry != nil {
		entry.request()
	}
}

// NewDispatcher wraps a handler so that workspace/executeCommand runs in its
// own goroutine and every other request is handled inline on the connection's
// read loop, exactly as before. Dispatch is selective rather than
// jsonrpc2.AsyncHandler so that exactly one method is concurrent and exactly
// the state it touches needs guarding.
func NewDispatcher(inner jsonrpc2.Handler) jsonrpc2.Handler {
	return &dispatcher{inner: inner}
}

type dispatcher struct {
	inner jsonrpc2.Handler
}

func (d *dispatcher) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Method == "workspace/executeCommand" {
		go d.inner.Handle(ctx, conn, req)
		return
	}
	d.inner.Handle(ctx, conn, req)
}
