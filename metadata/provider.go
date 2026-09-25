package metadata

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/catalog"
)

// Public errors returned by the registry, lease and detail APIs. Callers
// should use errors.Is against these values rather than parsing messages.
var (
	ErrClosed         = errors.New("metadata: registry or lease is closed")
	ErrStale          = errors.New("metadata: snapshot or detail is stale")
	ErrUnsupported    = errors.New("metadata: operation is not supported by this provider")
	ErrScopeRequired  = errors.New("metadata: an authenticated scope is required")
	ErrInvalidTarget  = errors.New("metadata: invalid database target")
	ErrDetailLimit    = errors.New("metadata: detail exceeds the retained size budget")
	ErrCategoryFailed = errors.New("metadata: a required category did not become ready")
)

// Key identifies one shared metadata store. Two connections that normalize to
// the same Key share one provider and one catalog cache.
type Key struct {
	Engine string
	URL    string
}

// Scope selects the authenticated visibility a lease observes under one Key.
// Equal scopes share a load; different scopes never merge, since permissions
// can differ per principal/role even against the identical database. Scope
// never carries a password or any other secret.
type Scope struct {
	Principal  string
	Role       string
	TextPolicy string
}

// Consistency identifies the read consistency a detail request tolerates.
type Consistency string

const (
	Committed          Consistency = "committed"
	TransactionLocal   Consistency = "transaction-local"
	ConsistentSnapshot Consistency = "consistent-snapshot"
)

// Request identifies one detail fetch: an object, a facet of that object, a
// rendering dialect (never database identity) and the tolerated consistency.
type Request struct {
	Object      catalog.ObjectID
	Facet       string
	Dialect     int
	Consistency Consistency
}

// Detail is one rendered object fragment: source text, provenance, columns
// and/or a structured table description, depending on the requested facet.
type Detail struct {
	Object      catalog.ObjectID
	Text        string
	Provenance  string
	Columns     []*catalog.ColumnDesc
	Description catalog.TableDescription
}

// DetailPlan describes how to load and render one detail request.
type DetailPlan struct {
	Requires []catalog.MetadataKind
	Coverage string
	// CoverageForCache optionally derives the store's acquisition key from
	// the exact ready cache snapshot whose categories were checked by Lease.
	// It must be pure and read-only: it may resolve canonical object identity,
	// but must not perform I/O or mutate cache state. When nil, Coverage is
	// used unchanged for backwards compatibility.
	CoverageForCache func(*catalog.Cache) (string, error)
	Load             func(context.Context, *catalog.Cache) (Detail, error)
	Render           func(*catalog.Cache, Detail, Request) (Detail, error)
}

// Provider is one engine's catalog-plan and detail-plan source for one
// (Key, Scope) store. The registry owns its lifecycle; Close is called at
// most once, when the last lease for that store is released.
type Provider interface {
	Plan() catalog.MetadataPlan
	DetailPlan(Request) (DetailPlan, error)
	Close() error
}

// Factory creates a Provider for one store. The registry invokes it with a
// registry-owned context: canceling a caller's Acquire context never cancels
// a Factory call still needed by another consumer.
type Factory func(context.Context) (Provider, error)

// Options bounds registry-wide resource usage.
type Options struct {
	MaxHostJobs    int
	MaxStoreJobs   int
	MaxDetailBytes int64
}

// Event announces a change to one store's published snapshot. Incarnation
// identifies the store instance (see Lease.Incarnation); Generation and
// Revision mirror the underlying catalog.MetadataSnapshot fields for that
// instance. Event never carries a password or any other secret.
type Event struct {
	Key         Key
	Incarnation uint64
	Generation  uint64
	Revision    uint64
}
