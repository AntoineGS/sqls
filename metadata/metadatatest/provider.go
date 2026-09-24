// Package metadatatest provides a deterministic metadata.Provider fixture
// shared by SQLs's and its embedding hosts' registry/lease tests.
package metadatatest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// Provider is a deterministic metadata.Provider fixture. Its relations job
// increments Calls exactly once per execution, signals Entered once, then
// waits on Gate (if non-nil) before returning Cache or Failure.
type Provider struct {
	Calls   atomic.Int64
	Closes  atomic.Int64
	Entered chan struct{}
	Gate    <-chan struct{}
	Cache   *catalog.Cache
	Failure error

	enteredOnce sync.Once
}

// New returns a Provider seeded with one "T" relation and an open Entered
// channel.
func New() *Provider {
	return &Provider{
		Entered: make(chan struct{}),
		Cache:   &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}},
	}
}

// Plan returns one relations job driven by Calls/Entered/Gate/Cache/Failure.
func (p *Provider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{
		Parallelism: 1,
		Jobs: []catalog.MetadataJob{
			{Kind: catalog.MetadataRelations, Run: p.runRelations},
		},
	}
}

func (p *Provider) runRelations(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	p.Calls.Add(1)
	p.enteredOnce.Do(func() { close(p.Entered) })
	if p.Gate != nil {
		select {
		case <-p.Gate:
		case <-ctx.Done():
		}
	}
	if p.Failure != nil {
		return catalog.MetadataPatch{}, p.Failure
	}
	return catalog.MetadataPatch{Cache: p.Cache, Count: len(p.Cache.SchemaTables[""])}, nil
}

// DetailPlan always reports metadata.ErrUnsupported; task A4 extends this
// fixture with configurable detail behavior.
func (p *Provider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{}, metadata.ErrUnsupported
}

// Close increments Closes and always succeeds.
func (p *Provider) Close() error {
	p.Closes.Add(1)
	return nil
}

// Factory adapts this fixture to metadata.Factory.
func (p *Provider) Factory(context.Context) (metadata.Provider, error) { return p, nil }

// WaitReady subscribes to lease before reading its snapshot, then waits for
// the snapshot to settle (every category reaches a terminal state) or for
// ctx/the subscription to end first.
func WaitReady(ctx context.Context, lease *metadata.Lease) error {
	sub := lease.Subscribe()
	defer sub.Close()
	for {
		snapshot := lease.Snapshot()
		if snapshot != nil {
			if snapshot.StartFailed {
				return fmt.Errorf("metadatatest: metadata start failed")
			}
			if snapshot.Settled() {
				return nil
			}
		}
		select {
		case _, ok := <-sub.C:
			if !ok {
				// The store was torn down before it ever settled.
				return fmt.Errorf("metadatatest: subscription closed before readiness")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
