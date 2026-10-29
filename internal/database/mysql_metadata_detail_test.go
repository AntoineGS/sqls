package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

func TestMySQLMetadataDetailSupportedFacetsAndCachedValues(t *testing.T) {
	state := newMySQLMetadataDetailScript()
	db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "MDE_FIXTURE"}
	cache := mysqlMetadataDetailCache()

	tests := []struct {
		name    string
		object  catalog.ObjectID
		facet   string
		wantReq []catalog.MetadataKind
		check   func(*testing.T, metadata.Detail)
	}{
		{
			name:    "table columns",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "odd`table"},
			facet:   "columns",
			wantReq: []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataViews},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if len(got.Columns) != 2 || got.Columns[0].Name != "first`column" || got.Columns[1].Name != "last_column" {
					t.Fatalf("table columns = %#v, want cached order [first`column last_column]", got.Columns)
				}
			},
		},
		{
			name:    "view columns",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "items_view"},
			facet:   "columns",
			wantReq: []catalog.MetadataKind{catalog.MetadataViews},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if len(got.Columns) != 1 || got.Columns[0].Name != "view_id" {
					t.Fatalf("view columns = %#v, want its cached view_id column", got.Columns)
				}
			},
		},
		{
			name:    "table description",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "odd`table"},
			facet:   "description",
			wantReq: []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataViews},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if len(got.Columns) != 2 {
					t.Fatalf("description columns = %d, want two cached columns", len(got.Columns))
				}
				quotedTable := "`odd``table`"
				if span := got.Description.Table; !validMySQLDescriptionSpan(got.Description.Body, span, quotedTable) {
					t.Fatalf("table span %+v does not point to %q in %q", span, quotedTable, got.Description.Body)
				}
				for _, column := range got.Description.Columns {
					quoted := "`" + strings.ReplaceAll(column.Name, "`", "``") + "`"
					if !validMySQLDescriptionSpan(got.Description.Body, column.Span, quoted) {
						t.Fatalf("column %q span %+v does not point to %q in %q", column.Name, column.Span, quoted, got.Description.Body)
					}
				}
			},
		},
		{
			name:    "view description",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "items_view"},
			facet:   "description",
			wantReq: []catalog.MetadataKind{catalog.MetadataViews},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if len(got.Columns) != 1 || got.Columns[0].Name != "view_id" || got.Description.Body == "" {
					t.Fatalf("view description = %+v, want its cached column and rendered body", got)
				}
			},
		},
		{
			name:    "view source",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "items_view"},
			facet:   "source",
			wantReq: []catalog.MetadataKind{catalog.MetadataViews},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if got.Text != "SELECT view_id FROM `odd``table`" {
					t.Fatalf("view source = %q", got.Text)
				}
			},
		},
		{
			name:    "procedure source",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "save_items"},
			facet:   "source",
			wantReq: []catalog.MetadataKind{catalog.MetadataProcedures},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if got.Text != "BEGIN SELECT 1; END" {
					t.Fatalf("procedure source = %q", got.Text)
				}
			},
		},
		{
			name:    "trigger source",
			object:  catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: "items_bi"},
			facet:   "source",
			wantReq: []catalog.MetadataKind{catalog.MetadataTriggers},
			check: func(t *testing.T, got metadata.Detail) {
				t.Helper()
				if got.Text != "SET NEW.email = LOWER(NEW.email)" {
					t.Fatalf("trigger source = %q", got.Text)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := repo.MetadataDetailPlan(metadata.Request{Object: test.object, Facet: test.facet, Consistency: metadata.Committed})
			if err != nil {
				t.Fatalf("MetadataDetailPlan() error = %v", err)
			}
			if !reflect.DeepEqual(plan.Requires, test.wantReq) {
				t.Fatalf("Requires = %v, want %v", plan.Requires, test.wantReq)
			}
			got, err := plan.Load(context.Background(), cache)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got.Object.Schema != cache.DefaultSchema {
				t.Fatalf("detail object schema = %q, want canonical cache schema %q", got.Object.Schema, cache.DefaultSchema)
			}
			test.check(t, got)
		})
	}
}

func TestMySQLMetadataDetailUnsupportedFacetsAndConsistency(t *testing.T) {
	repo := &MySQLMetadataRepository{Schema: "mde_fixture"}
	kinds := []catalog.ObjectKind{
		catalog.ObjectKindTable, catalog.ObjectKindView, catalog.ObjectKindProcedure,
		catalog.ObjectKindFunction, catalog.ObjectKindTrigger, catalog.ObjectKindDomain,
		catalog.ObjectKindIndex, catalog.ObjectKindGenerator, catalog.ObjectKindException,
	}
	for _, kind := range kinds {
		for _, facet := range []string{"columns", "description", "source", "ddl"} {
			supported := false
			switch facet {
			case "columns", "description":
				supported = kind == catalog.ObjectKindTable || kind == catalog.ObjectKindView
			case "source":
				supported = kind == catalog.ObjectKindView || kind == catalog.ObjectKindProcedure || kind == catalog.ObjectKindTrigger
			case "ddl":
				supported = kind == catalog.ObjectKindTable || kind == catalog.ObjectKindView || kind == catalog.ObjectKindProcedure || kind == catalog.ObjectKindFunction || kind == catalog.ObjectKindTrigger
			}
			if supported {
				continue
			}
			_, err := repo.MetadataDetailPlan(metadata.Request{Object: catalog.ObjectID{Kind: kind, Name: "object"}, Facet: facet, Consistency: metadata.Committed})
			if !errors.Is(err, metadata.ErrUnsupported) {
				t.Errorf("MetadataDetailPlan(%s, %s) error = %v, want metadata.ErrUnsupported", kind, facet, err)
			}
		}
	}
	for _, consistency := range []metadata.Consistency{metadata.TransactionLocal, metadata.ConsistentSnapshot} {
		_, err := repo.MetadataDetailPlan(metadata.Request{
			Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "odd`table"},
			Facet:  "columns", Consistency: consistency,
		})
		if !errors.Is(err, metadata.ErrUnsupported) {
			t.Errorf("MetadataDetailPlan(consistency=%s) error = %v, want metadata.ErrUnsupported", consistency, err)
		}
	}
}

func TestMySQLMetadataDetailDDLUsesExactSHOWCreateAndQuotedIdentifiers(t *testing.T) {
	state := newMySQLMetadataDetailScript()
	db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "MDE_FIXTURE"}
	cache := mysqlMetadataDetailCache()

	cases := []struct {
		kind                  catalog.ObjectKind
		name, statement, want string
	}{
		{catalog.ObjectKindTable, "odd`table", "SHOW CREATE TABLE `mde_fixture`.`odd``table`", "CREATE TABLE `odd``table` (\n  `first``column` varchar(40) NOT NULL\n)"},
		{catalog.ObjectKindView, "items_view", "SHOW CREATE VIEW `mde_fixture`.`items_view`", "CREATE VIEW `items_view` AS SELECT 1"},
		{catalog.ObjectKindProcedure, "save_items", "SHOW CREATE PROCEDURE `mde_fixture`.`save_items`", "CREATE PROCEDURE `save_items`() BEGIN SELECT 1; END"},
		{catalog.ObjectKindFunction, "item_count", "SHOW CREATE FUNCTION `mde_fixture`.`item_count`", "CREATE FUNCTION `item_count`() RETURNS BIGINT RETURN 1"},
		{catalog.ObjectKindTrigger, "items_bi", "SHOW CREATE TRIGGER `mde_fixture`.`items_bi`", "CREATE DEFINER=`root`@`localhost` TRIGGER `items_bi` BEFORE INSERT ON `items` FOR EACH ROW SET NEW.email = LOWER(NEW.email)"},
	}
	for _, test := range cases {
		t.Run(string(test.kind), func(t *testing.T) {
			plan, err := repo.MetadataDetailPlan(metadata.Request{
				Object: catalog.ObjectID{Schema: "mde_fixture", Kind: test.kind, Name: test.name}, Facet: "ddl", Consistency: metadata.Committed,
			})
			if err != nil {
				t.Fatalf("MetadataDetailPlan() error = %v", err)
			}
			if test.kind == catalog.ObjectKindTable && !reflect.DeepEqual(plan.Requires, []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataViews}) {
				t.Errorf("table DDL Requires = %v, want relations and views for relation-kind classification", plan.Requires)
			}
			got, err := plan.Load(context.Background(), cache)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got.Text != test.want {
				t.Fatalf("DDL = %q, want DDL from the named SHOW CREATE result column %q", got.Text, test.want)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if len(state.queries) != 1 || state.queries[0] != test.statement {
				t.Fatalf("queries = %q, want exactly %q", state.queries, test.statement)
			}
			if len(state.options) != 1 || !state.options[0].ReadOnly || state.options[0].Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
				t.Fatalf("transaction options = %+v, want one read-only read-committed transaction", state.options)
			}
			if state.commits != 1 || state.rollbacks != 0 {
				t.Fatalf("transaction commits=%d rollbacks=%d, want 1/0", state.commits, state.rollbacks)
			}
			state.reset()
		})
	}
}

func TestMySQLMetadataDetailRejectsOtherSchemaBeforeQuery(t *testing.T) {
	state := newMySQLMetadataDetailScript()
	db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "MDE_FIXTURE"}
	plan, err := repo.MetadataDetailPlan(metadata.Request{
		Object: catalog.ObjectID{Schema: "mde_other", Kind: catalog.ObjectKindTable, Name: "odd`table"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("MetadataDetailPlan() error = %v", err)
	}
	_, err = plan.Load(context.Background(), mysqlMetadataDetailCache())
	if !errors.Is(err, metadata.ErrUnsupported) {
		t.Fatalf("Load(other schema) error = %v, want metadata.ErrUnsupported", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.queries) != 0 {
		t.Fatalf("other-schema request ran queries %q, want none", state.queries)
	}
}

func TestMySQLMetadataDetailRejectsNonCanonicalSchemaCaseBeforeQuery(t *testing.T) {
	state := newMySQLMetadataDetailScript()
	db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "MDE_FIXTURE"}
	plan, err := repo.MetadataDetailPlan(metadata.Request{
		Object: catalog.ObjectID{Schema: "MDE_FIXTURE", Kind: catalog.ObjectKindTable, Name: "odd`table"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("MetadataDetailPlan() error = %v", err)
	}
	_, err = plan.Load(context.Background(), mysqlMetadataDetailCache())
	if !errors.Is(err, metadata.ErrUnsupported) {
		t.Fatalf("Load(non-canonical schema case) error = %v, want metadata.ErrUnsupported", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.queries) != 0 {
		t.Fatalf("non-canonical schema request ran queries %q, want none", state.queries)
	}
}

func TestMySQLMetadataDetailResolvesBeforeDDLQuery(t *testing.T) {
	state := newMySQLMetadataDetailScript()
	db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
	t.Cleanup(func() { _ = db.Close() })
	repo := &MySQLMetadataRepository{Conn: db, Schema: "mde_fixture"}
	plan, err := repo.MetadataDetailPlan(metadata.Request{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "missing"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("MetadataDetailPlan() error = %v", err)
	}
	_, err = plan.Load(context.Background(), mysqlMetadataDetailCache())
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Load(unknown table) error = %v, want ErrObjectNotFound", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.queries) != 0 {
		t.Fatalf("unknown object ran DDL queries %q, want none", state.queries)
	}
}

func TestMySQLMetadataDetailRejectsWrongRelationKindBeforeDDLQuery(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      catalog.ObjectKind
		nameValue string
	}{
		{name: "table request for view", kind: catalog.ObjectKindTable, nameValue: "items_view"},
		{name: "view request for table", kind: catalog.ObjectKindView, nameValue: "odd`table"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newMySQLMetadataDetailScript()
			db := sql.OpenDB(mysqlMetadataDetailConnector{state: state})
			t.Cleanup(func() { _ = db.Close() })
			repo := &MySQLMetadataRepository{Conn: db, Schema: "mde_fixture"}
			plan, err := repo.MetadataDetailPlan(metadata.Request{
				Object: catalog.ObjectID{Kind: test.kind, Name: test.nameValue},
				Facet:  "ddl", Consistency: metadata.Committed,
			})
			if err != nil {
				t.Fatalf("MetadataDetailPlan() error = %v", err)
			}
			_, err = plan.Load(context.Background(), mysqlMetadataDetailCache())
			if !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("Load(wrong relation kind) error = %v, want ErrObjectNotFound", err)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if len(state.queries) != 0 {
				t.Fatalf("wrong relation kind issued DDL queries %q", state.queries)
			}
		})
	}
}

func mysqlMetadataDetailCache() *catalog.Cache {
	tableColumns := []*catalog.ColumnDesc{
		{ColumnBase: catalog.ColumnBase{Schema: "mde_fixture", Table: "odd`table", Name: "first`column"}, Type: "varchar(40)", Null: "NO"},
		{ColumnBase: catalog.ColumnBase{Schema: "mde_fixture", Table: "odd`table", Name: "last_column"}, Type: "bigint unsigned", Null: "YES"},
	}
	viewColumns := []*catalog.ColumnDesc{
		{ColumnBase: catalog.ColumnBase{Schema: "mde_fixture", Table: "items_view", Name: "view_id"}, Type: "bigint unsigned", Null: "NO"},
	}
	view := &catalog.ViewDesc{Schema: "mde_fixture", Name: "items_view", ViewSource: sql.NullString{String: "SELECT view_id FROM `odd``table`", Valid: true}, Columns: viewColumns}
	procedure := &catalog.ProcedureDesc{Schema: "mde_fixture", Name: "save_items", Source: sql.NullString{String: "BEGIN SELECT 1; END", Valid: true}}
	function := &catalog.FunctionDesc{Schema: "mde_fixture", Name: "item_count"}
	trigger := &catalog.TriggerDesc{Schema: "mde_fixture", Name: "items_bi", Source: sql.NullString{String: "SET NEW.email = LOWER(NEW.email)", Valid: true}}
	return &catalog.Cache{
		DefaultSchema: "mde_fixture",
		SchemaTables:  map[string][]string{"MDE_FIXTURE": {"odd`table", "items_view"}},
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "odd`table"}).Key():  tableColumns,
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "items_view"}).Key(): viewColumns,
		},
		Catalog: &catalog.CatalogCache{
			ViewsByID:      map[string]*catalog.ViewDesc{(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "items_view"}).Key(): view},
			ProceduresByID: map[string]*catalog.ProcedureDesc{(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "save_items"}).Key(): procedure},
			FunctionsByID:  map[string]*catalog.FunctionDesc{(catalog.ObjectID{Kind: catalog.ObjectKindFunction, Name: "item_count"}).Key(): function},
			TriggersByID:   map[string]*catalog.TriggerDesc{(catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: "items_bi"}).Key(): trigger},
		},
	}
}

func validMySQLDescriptionSpan(body string, span catalog.DescriptionSpan, want string) bool {
	return span.Start >= 0 && span.End > span.Start && span.End <= len(body) && body[span.Start:span.End] == want
}

type mysqlMetadataDetailScript struct {
	mu        sync.Mutex
	queries   []string
	options   []driver.TxOptions
	commits   int
	rollbacks int
}

func newMySQLMetadataDetailScript() *mysqlMetadataDetailScript { return &mysqlMetadataDetailScript{} }

func (s *mysqlMetadataDetailScript) reset() {
	s.queries = nil
	s.options = nil
	s.commits = 0
	s.rollbacks = 0
}

type mysqlMetadataDetailConnector struct{ state *mysqlMetadataDetailScript }

func (c mysqlMetadataDetailConnector) Connect(context.Context) (driver.Conn, error) {
	return mysqlMetadataDetailConn{state: c.state}, nil
}
func (mysqlMetadataDetailConnector) Driver() driver.Driver { return mysqlMetadataDetailDriver{} }

type mysqlMetadataDetailDriver struct{}

func (mysqlMetadataDetailDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("not used")
}

type mysqlMetadataDetailConn struct{ state *mysqlMetadataDetailScript }

func (mysqlMetadataDetailConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (mysqlMetadataDetailConn) Close() error                        { return nil }
func (c mysqlMetadataDetailConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c mysqlMetadataDetailConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	c.state.options = append(c.state.options, options)
	c.state.mu.Unlock()
	return mysqlMetadataDetailTx{state: c.state}, nil
}
func (c mysqlMetadataDetailConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.queries = append(c.state.queries, query)
	c.state.mu.Unlock()
	return mysqlMetadataDetailRowsFor(query)
}

type mysqlMetadataDetailTx struct{ state *mysqlMetadataDetailScript }

func (tx mysqlMetadataDetailTx) Commit() error {
	tx.state.mu.Lock()
	tx.state.commits++
	tx.state.mu.Unlock()
	return nil
}
func (tx mysqlMetadataDetailTx) Rollback() error {
	tx.state.mu.Lock()
	tx.state.rollbacks++
	tx.state.mu.Unlock()
	return nil
}

type mysqlMetadataDetailRows struct {
	columns []string
	values  []driver.Value
	read    bool
}

func (r *mysqlMetadataDetailRows) Columns() []string { return r.columns }
func (*mysqlMetadataDetailRows) Close() error        { return nil }
func (r *mysqlMetadataDetailRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	copy(dest, r.values)
	return nil
}

func mysqlMetadataDetailRowsFor(query string) (driver.Rows, error) {
	var columns []string
	var values []driver.Value
	switch {
	case query == "SHOW CREATE TABLE `mde_fixture`.`odd``table`":
		columns = []string{"Table", "Create Table", "character_set_client", "collation_connection"}
		values = []driver.Value{"odd`table", "CREATE TABLE `odd``table` (\n  `first``column` varchar(40) NOT NULL\n)", "utf8mb4", "utf8mb4_0900_ai_ci"}
	case query == "SHOW CREATE VIEW `mde_fixture`.`items_view`":
		columns = []string{"View", "Create View", "character_set_client", "collation_connection"}
		values = []driver.Value{"items_view", "CREATE VIEW `items_view` AS SELECT 1", "utf8mb4", "utf8mb4_0900_ai_ci"}
	case query == "SHOW CREATE PROCEDURE `mde_fixture`.`save_items`":
		columns = []string{"Procedure", "sql_mode", "Create Procedure", "character_set_client", "collation_connection", "Database Collation"}
		values = []driver.Value{"save_items", "STRICT_TRANS_TABLES", "CREATE PROCEDURE `save_items`() BEGIN SELECT 1; END", "utf8mb4", "utf8mb4_0900_ai_ci", "utf8mb4_0900_ai_ci"}
	case query == "SHOW CREATE FUNCTION `mde_fixture`.`item_count`":
		columns = []string{"Function", "sql_mode", "Create Function", "character_set_client", "collation_connection", "Database Collation"}
		values = []driver.Value{"item_count", "STRICT_TRANS_TABLES", "CREATE FUNCTION `item_count`() RETURNS BIGINT RETURN 1", "utf8mb4", "utf8mb4_0900_ai_ci", "utf8mb4_0900_ai_ci"}
	case query == "SHOW CREATE TRIGGER `mde_fixture`.`items_bi`":
		columns = []string{"Trigger", "sql_mode", "SQL Original Statement", "character_set_client", "collation_connection", "Database Collation"}
		values = []driver.Value{"items_bi", "STRICT_TRANS_TABLES", "CREATE DEFINER=`root`@`localhost` TRIGGER `items_bi` BEFORE INSERT ON `items` FOR EACH ROW SET NEW.email = LOWER(NEW.email)", "utf8mb4", "utf8mb4_0900_ai_ci", "utf8mb4_0900_ai_ci"}
	default:
		return nil, errors.New("unexpected MySQL detail query: " + query)
	}
	return &mysqlMetadataDetailRows{columns: columns, values: values}, nil
}

var _ driver.Connector = mysqlMetadataDetailConnector{}
var _ driver.ConnBeginTx = mysqlMetadataDetailConn{}
var _ driver.QueryerContext = mysqlMetadataDetailConn{}
