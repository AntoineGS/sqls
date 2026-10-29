package mymeta

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
)

func TestFactoryRejectsInvalidTargetBeforeOpening(t *testing.T) {
	factory := Factory(Config{Target: "not-a-target", User: "alice"})
	provider, err := factory(context.Background())
	if provider != nil {
		t.Fatalf("Factory(invalid target) returned provider %T", provider)
	}
	if !errors.Is(err, metadata.ErrInvalidTarget) {
		t.Fatalf("Factory(invalid target) error = %v, want metadata.ErrInvalidTarget", err)
	}
}

func TestProviderDelegatesPlansAndOwnsPool(t *testing.T) {
	var gotDriver, gotDSN string
	var connects atomic.Int64
	var candidate *sql.DB
	provider, err := newProviderWithOpen(context.Background(), Config{
		Target: "alice@localhost:3307/mde_fixture", Password: "secret",
	}, func(driverName, dsn string) (*sql.DB, error) {
		gotDriver, gotDSN = driverName, dsn
		candidate = sql.OpenDB(mysqlProviderConnector{connects: &connects})
		return candidate, nil
	})
	if err != nil {
		t.Fatalf("newProviderWithOpen() error = %v", err)
	}
	p, ok := provider.(*mysqlProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *mysqlProvider", provider)
	}
	if gotDriver != "mysql" || !strings.Contains(gotDSN, "alice:secret@tcp(localhost:3307)/mde_fixture") {
		t.Fatalf("open arguments = (%q, %q), want mysql driver and configured DSN", gotDriver, gotDSN)
	}
	if got := p.conn.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("MaxOpenConnections = %d, want 3", got)
	}
	plan := provider.Plan()
	wantPlan := (&database.MySQLMetadataRepository{Conn: p.conn, Schema: "mde_fixture"}).MetadataPlan()
	if len(plan.Jobs) != len(wantPlan.Jobs) || plan.Parallelism != 3 {
		t.Fatalf("Plan() = %d jobs / parallelism %d, want %d / 3", len(plan.Jobs), plan.Parallelism, len(wantPlan.Jobs))
	}
	for i := range plan.Jobs {
		if plan.Jobs[i].Kind != wantPlan.Jobs[i].Kind {
			t.Fatalf("Plan() job[%d] = %s, want delegated repository kind %s", i, plan.Jobs[i].Kind, wantPlan.Jobs[i].Kind)
		}
	}
	detailPlan, err := provider.DetailPlan(metadata.Request{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "items"},
		Facet:  "columns", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("DetailPlan() error = %v", err)
	}
	if len(detailPlan.Requires) != 2 || detailPlan.Requires[0] != catalog.MetadataRelations || detailPlan.Requires[1] != catalog.MetadataColumnsCurrent {
		t.Fatalf("DetailPlan().Requires = %v, want repository's table-column dependencies", detailPlan.Requires)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := candidate.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("PingContext after provider Close() = %v, want closed database error", err)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("connector Connect calls = %d, want only the setup Ping", got)
	}
}

func TestProviderPingFailureClosesCandidatePool(t *testing.T) {
	wantErr := errors.New("deterministic connector failure")
	var connects atomic.Int64
	var candidate *sql.DB
	provider, err := newProviderWithOpen(context.Background(), Config{Target: "localhost/mde_fixture", User: "alice"}, func(string, string) (*sql.DB, error) {
		candidate = sql.OpenDB(mysqlProviderFailingConnector{err: wantErr, connects: &connects})
		return candidate, nil
	})
	if provider != nil {
		t.Fatalf("newProviderWithOpen() returned provider %T after Ping failure", provider)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("newProviderWithOpen() error = %v, want connector error %v", err, wantErr)
	}
	if candidate == nil {
		t.Fatal("open function did not capture the candidate pool")
	}
	if err := candidate.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("PingContext after setup failure = %v, want closed database error", err)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("connector Connect calls after post-failure ping = %d, want 1", got)
	}
}

type mysqlProviderConnector struct{ connects *atomic.Int64 }

func (c mysqlProviderConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return mysqlProviderConn{}, nil
}
func (mysqlProviderConnector) Driver() driver.Driver { return mysqlProviderDriver{} }

type mysqlProviderFailingConnector struct {
	err      error
	connects *atomic.Int64
}

func (c mysqlProviderFailingConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return nil, c.err
}
func (mysqlProviderFailingConnector) Driver() driver.Driver { return mysqlProviderDriver{} }

type mysqlProviderDriver struct{}

func (mysqlProviderDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

type mysqlProviderConn struct{}

func (mysqlProviderConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (mysqlProviderConn) Close() error                        { return nil }
func (mysqlProviderConn) Begin() (driver.Tx, error)           { return nil, errors.New("not used") }

var _ driver.Connector = mysqlProviderConnector{}
var _ driver.Connector = mysqlProviderFailingConnector{}
