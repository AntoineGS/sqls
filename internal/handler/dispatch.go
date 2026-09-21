package handler

import (
	"context"

	"github.com/sourcegraph/jsonrpc2"
)

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
