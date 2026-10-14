package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
	"interbase-go/txcontext"
	"reflect"
	"testing"
	"time"
)

func TestWatchedFullReadCommittedGroup(t *testing.T) {
	state := &metadataTxState{width: int64(127)}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	patch, err := repo.runWatchedRead(context.Background(), func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
		if width != 127 {
			t.Fatalf("width=%d", width)
		}
		rows, err := q.QueryContext(ctx, "group read")
		if err != nil {
			return MetadataPatch{}, err
		}
		return MetadataPatch{Count: 1, Cache: &DBCache{}}, rows.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.options.ReadOnly || state.options.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) || state.begins != 1 || state.rollbacks != 1 || state.opens != 1 || !state.queryInTx || patch.Queries != 2 {
		t.Fatalf("patch=%+v state=%+v", patch, state)
	}
}

func TestWatchedBeginUsesOriginalAttemptAdmission(t *testing.T) {
	type valueKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), valueKey{}, "attempt-value"), time.Hour)
	defer cancel()
	state := &metadataTxState{width: int64(127)}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	_, err := repo.runWatchedRead(ctx, func(ctx context.Context, q schema.Queryer, _ int) (MetadataPatch, error) {
		rows, err := q.QueryContext(ctx, "data read")
		if err != nil {
			return MetadataPatch{}, err
		}
		return MetadataPatch{Cache: &DBCache{}}, rows.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := txcontext.BeginAdmission(state.beginContext)
	if admission != ctx {
		t.Fatal("native Begin admission does not retain original attempt context")
	}
	if state.beginContext.Value(valueKey{}) != "attempt-value" {
		t.Fatal("transaction context values lost")
	}
	want, _ := ctx.Deadline()
	got, ok := admission.Deadline()
	if !ok || got != want {
		t.Fatal("begin admission deadline changed")
	}
	for _, qctx := range state.queryContexts {
		if qctx != ctx {
			t.Fatal("query acquired a separate per-group context/budget")
		}
	}
	if state.begins != 1 || state.rollbacks != 1 || repo.Conn.Stats().InUse != 0 {
		t.Fatal("group did not synchronously end/release")
	}
}

func TestWatchedBeginSuccessfulCompletionCancellationEndsExplicitly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("terminal rollback failure")
	state := &metadataTxState{width: int64(127), cancelOnBegin: cancel, rollbackErr: failure}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	patch, err := repo.runWatchedRead(ctx, func(context.Context, schema.Queryer, int) (MetadataPatch, error) {
		t.Error("read ran after begin completion cancellation")
		return MetadataPatch{Cache: &DBCache{}}, nil
	})
	if patch.Cache != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, failure) || state.begins != 1 || state.rollbacks != 1 || state.queries != 0 || repo.Conn.Stats().InUse != 0 {
		t.Fatalf("patch=%+v err=%v begins=%d ends=%d queries=%d", patch, err, state.begins, state.rollbacks, state.queries)
	}
}

func TestWatchedFullParity(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	db.SetMaxOpenConns(1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3, SourceSQLDialect: 3}
	loader := metadata.NewLoader(nil)
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, repo.MetadataPlan())
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	want := loader.Snapshot()
	got, status, metrics, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertWatchedCacheParity(t, got, want.Cache)
	if metrics.Queries <= 0 || metrics.Duration <= 0 {
		t.Fatalf("metrics=%+v", metrics)
	}
	for kind, s := range want.Status {
		if kind == MetadataColumnsAll {
			continue
		}
		g := status[kind]
		if g.State != MetadataReady || g.Count != s.Count || !g.QueriesKnown {
			t.Fatalf("%s got=%+v want=%+v", kind, g, s)
		}
	}
}

func assertWatchedCacheParity(t *testing.T, got, want *DBCache) {
	t.Helper()
	// ObjectIndex contains a folding function (functions are never DeepEqual).
	// Compare its observable exact/folded lookups, then all descriptor fields.
	gc, wc := *got, *want
	gcat, wcat := *got.Catalog, *want.Catalog
	for _, c := range []*CatalogCache{&gcat, &wcat} {
		v := reflect.ValueOf(c).Elem()
		fields := map[string]catalog.ObjectKind{"ViewsByID": catalog.ObjectKindView, "ProceduresByID": catalog.ObjectKindProcedure, "GeneratorsByID": catalog.ObjectKindGenerator, "DomainsByID": catalog.ObjectKindDomain, "FunctionsByID": catalog.ObjectKindFunction, "IndexesByID": catalog.ObjectKindIndex, "TriggersByID": catalog.ObjectKindTrigger, "SystemRelationsByID": catalog.ObjectKindTable, "ExceptionsByID": catalog.ObjectKindException}
		for field, kind := range fields {
			iter := v.FieldByName(field).MapRange()
			for iter.Next() {
				desc := iter.Value()
				id := catalog.ObjectID{Kind: kind, Name: desc.Elem().FieldByName("Name").String()}
				g, gok := gcat.Objects.Lookup(id)
				w, wok := wcat.Objects.Lookup(id)
				if !gok || !wok || g != w {
					t.Fatal("exact object lookup differs")
				}
				if !reflect.DeepEqual(gcat.Objects.LookupUnquoted(id.Kind, id.Name), wcat.Objects.LookupUnquoted(id.Kind, id.Name)) {
					t.Fatal("folded object lookup differs")
				}
			}
		}
	}
	gcat.Objects, wcat.Objects = nil, nil
	gc.Catalog, wc.Catalog = &gcat, &wcat
	if !reflect.DeepEqual(gc, wc) {
		t.Fatal("complete cache descriptors differ from independent loader")
	}
}

func TestWatchedFullCleanupAndCancellation(t *testing.T) {
	failure := errors.New("cleanup failure")
	state := &metadataTxState{width: int64(127), rollbackErr: failure}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	patch, err := repo.runWatchedRead(ctx, func(context.Context, schema.Queryer, int) (MetadataPatch, error) {
		cancel()
		return MetadataPatch{Cache: &DBCache{}}, nil
	})
	if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || patch.Cache != nil || state.rollbacks != 1 {
		t.Fatalf("patch=%+v err=%v rollbacks=%d", patch, err, state.rollbacks)
	}
}

func TestWatchedFullTimeoutBeforeRead(t *testing.T) {
	state := &metadataTxState{width: int64(127)}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cache, _, _, err := repo.ReadWatchedFull(ctx)
	if cache != nil || !errors.Is(err, context.DeadlineExceeded) || state.begins != 0 {
		t.Fatalf("cache=%v err=%v begins=%d", cache, err, state.begins)
	}
}

func TestWatchedFullSerialTransactionsAndQueryAccounting(t *testing.T) {
	fault := &interBaseFixtureFault{}
	db := openInterBaseScalableFixtureWithFault(t, 2, fault)
	db.SetMaxOpenConns(1)
	fault.resetTransactions()
	var queries, widths int
	fault.onQuery = func(query string) {
		options, commits, rollbacks := fault.transactionSnapshot()
		if len(options)-commits-rollbacks != 1 {
			t.Errorf("query not owned by exactly one group")
		}
		queries++
		if query == interBaseMetadataIdentifierWidthQuery {
			widths++
		}
	}
	repo := &InterBaseDBRepository{Conn: db}
	cache, status, metrics, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	options, commits, rollbacks := fault.transactionSnapshot()
	if len(options) != 14 || commits != 0 || rollbacks != 14 || widths != 14 || metrics.Queries != int64(queries) || db.Stats().InUse != 0 {
		t.Fatalf("begins=%d commits=%d rollbacks=%d widths=%d queries=%d metrics=%+v", len(options), commits, rollbacks, widths, queries, metrics)
	}
	for _, o := range options {
		if !o.ReadOnly || o.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
			t.Fatalf("options=%+v", o)
		}
	}
	for kind, s := range status {
		if kind == MetadataColumnsAll {
			if s.State != MetadataUnsupported {
				t.Fatal("columns-all state")
			}
			continue
		}
		if s.State != MetadataReady || !cache.MetadataReady(kind) || !s.QueriesKnown {
			t.Fatalf("%s status=%+v", kind, s)
		}
	}
}

func TestWatchedFullRequiredCategoryFailures(t *testing.T) {
	for _, tc := range []struct {
		kind  MetadataKind
		match string
	}{
		{MetadataRelations, "FROM RDB$RELATIONS r\nWHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY"},
		{MetadataColumnsCurrent, "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY rf.RDB$RELATION_NAME"},
		{MetadataPrimaryKeys, "pk.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'"},
		{MetadataViews, "r.RDB$VIEW_BLR IS NOT NULL"},
		{MetadataDependencies, "FROM RDB$DEPENDENCIES d"},
		{MetadataIndexes, "FROM RDB$INDICES i\nLEFT JOIN RDB$RELATION_CONSTRAINTS"},
		{MetadataForeignKeys, "fk.RDB$CONSTRAINT_TYPE = 'FOREIGN KEY'"},
		{MetadataProcedures, "FROM RDB$PROCEDURES p"},
		{MetadataFunctions, "FROM RDB$FUNCTIONS f"},
		{MetadataGenerators, "RDB$GENERATORS"},
		{MetadataSystemRelations, "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) <> 0"},
		{MetadataExceptions, "FROM RDB$EXCEPTIONS e"},
		{MetadataDomains, "FROM RDB$FIELDS f\n"},
		{MetadataTriggers, "RDB$TRIGGERS"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			failure := errors.New("required category failure")
			fault := &interBaseFixtureFault{match: tc.match, stage: "query", err: failure}
			db := openInterBaseScalableFixtureWithFault(t, 2, fault)
			fault.resetTransactions()
			cache, status, metrics, err := (&InterBaseDBRepository{Conn: db}).ReadWatchedFull(context.Background())
			if cache != nil || !errors.Is(err, failure) || !errors.Is(err, metadata.ErrWatchIncomplete) || status[tc.kind].State != MetadataFailed || metrics.Queries <= 0 {
				t.Fatalf("category=%s state=%s err=%v", tc.kind, status[tc.kind].State, err)
			}
			options, commits, rollbacks := fault.transactionSnapshot()
			if len(options) != commits+rollbacks || db.Stats().InUse != 0 {
				t.Fatal("transaction/connection escaped failed category")
			}
		})
	}
}

func TestWatchedFullCanceledReadWaitsDrain(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	fault := &interBaseFixtureFault{match: "FROM RDB$RELATIONS r\nWHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY", gate: release, entered: entered}
	db := openInterBaseScalableFixtureWithFault(t, 1, fault)
	fault.resetTransactions()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		cache, _, _, err := (&InterBaseDBRepository{Conn: db}).ReadWatchedFull(ctx)
		if cache != nil {
			done <- errors.New("canceled read retained cache")
		} else {
			done <- err
		}
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before native-style query drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	options, commits, rollbacks := fault.transactionSnapshot()
	if len(options) != 1 || commits+rollbacks != 0 || db.Stats().InUse != 1 {
		t.Fatal("transaction released before drain")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	options, commits, rollbacks = fault.transactionSnapshot()
	if len(options) != 1 || commits+rollbacks != 1 || db.Stats().InUse != 0 {
		t.Fatal("cancellation failed to drain/end group")
	}
}

func TestWatchedFullDeadlineBetweenGroups(t *testing.T) {
	fault := &interBaseFixtureFault{}
	db := openInterBaseScalableFixtureWithFault(t, 1, fault)
	fault.resetTransactions()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// The driver has ended the relations transaction before this test-only
	// boundary hook waits for expiry. No columns group may start afterward.
	fault.onTxFinish = func() { <-ctx.Done() }
	cache, _, _, err := (&InterBaseDBRepository{Conn: db}).ReadWatchedFull(ctx)
	if cache != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline cancellation not propagated")
	}
	options, commits, rollbacks := fault.transactionSnapshot()
	if len(options) != 1 || commits+rollbacks != 1 {
		t.Fatal("new category started after cancellation")
	}
}

func TestWatchedFullCancellationWaitsTerminalRollback(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 1)
	state := &metadataTxState{width: int64(127), rollbackGate: release, rollbackEntered: entered}
	repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repo.runWatchedRead(ctx, func(context.Context, schema.Queryer, int) (MetadataPatch, error) {
			cancel()
			return MetadataPatch{Cache: &DBCache{}}, nil
		})
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("returned before terminal native rollback: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if state.begins != 1 || state.rollbacks != 1 || repo.Conn.Stats().InUse != 0 {
		t.Fatal("rollback did not drain/release")
	}
}

func TestWatchedFullRowFaultsJoinRollbackFailure(t *testing.T) {
	readFailure, cleanupFailure := errors.New("read failure"), errors.New("rollback failure")
	for _, stage := range []string{"query", "scan", "iteration", "close"} {
		t.Run(stage, func(t *testing.T) {
			state := &metadataTxState{width: int64(127), rollbackErr: cleanupFailure}
			switch stage {
			case "query":
				state.queryErr = readFailure
			case "scan":
				state.width = "not-an-integer"
			case "iteration":
				state.nextErr = readFailure
			case "close":
				state.closeErr = readFailure
			}
			repo := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
			patch, err := repo.runWatchedRead(context.Background(), func(ctx context.Context, q schema.Queryer, _ int) (MetadataPatch, error) {
				rows, err := q.QueryContext(ctx, "data read")
				if err != nil {
					return MetadataPatch{}, err
				}
				return MetadataPatch{Cache: &DBCache{}}, rows.Close()
			})
			if err == nil || !errors.Is(err, cleanupFailure) || patch.Cache != nil || state.rollbacks != 1 || !patch.QueriesKnown || repo.Conn.Stats().InUse != 0 {
				t.Fatalf("stage=%s patch=%+v err=%v", stage, patch, err)
			}
			if stage != "scan" && !errors.Is(err, readFailure) {
				t.Fatal("primary read error lost")
			}
		})
	}
}
