package database

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser/parseutil"
)

var (
	ErrNotImplementation error = errors.New("not implementation")
)

const (
	DefaultMaxIdleConns = 10
	DefaultMaxOpenConns = 5
)

type DBRepository interface {
	Driver() dialect.DatabaseDriver
	CurrentDatabase(ctx context.Context) (string, error)
	Databases(ctx context.Context) ([]string, error)
	CurrentSchema(ctx context.Context) (string, error)
	Schemas(ctx context.Context) ([]string, error)
	SchemaTables(ctx context.Context) (map[string][]string, error)
	DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error)
	DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error)
	Exec(ctx context.Context, query string) (sql.Result, error)
	Query(ctx context.Context, query string) (*sql.Rows, error)
	DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error)
}

// ReadOnlyQuerier is an optional repository capability: a repository that can
// run a read statement inside an explicit read-only transaction and materialise
// the whole result itself. Handlers type-assert it rather than checking the
// driver name, so any driver that later implements it gets the behaviour for
// free.
type ReadOnlyQuerier interface {
	QueryReadOnly(ctx context.Context, query string) (*QueryResult, error)
}

type DBOption struct {
	MaxIdleConns int
	MaxOpenConns int
}

type fkItemDesc struct {
	fkID      string
	schema    string
	table     string
	column    string
	refTable  string
	refColumn string
}

func ColumnDoc(tableName string, colDesc *ColumnDesc) string {
	return catalog.ColumnDoc(tableName, colDesc)
}

func Coalesce(str ...string) string { return catalog.Coalesce(str...) }

func TableDoc(tableName string, cols []*ColumnDesc) string { return catalog.TableDoc(tableName, cols) }

func SubqueryDoc(name string, views []*parseutil.SubQueryView, dbCache *DBCache) string {
	return catalog.SubqueryDoc(name, views, dbCache)
}

func SubqueryColumnDoc(identName string, views []*parseutil.SubQueryView, dbCache *DBCache) string {
	return catalog.SubqueryColumnDoc(identName, views, dbCache)
}

func parseForeignKeys(rows *sql.Rows, schemaName string) ([]*ForeignKey, error) {
	var retVal []*ForeignKey
	var prevFk string
	var cur *ForeignKey
	for rows.Next() {
		var fkItem fkItemDesc
		err := rows.Scan(
			&fkItem.fkID,
			&fkItem.table,
			&fkItem.column,
			&fkItem.refTable,
			&fkItem.refColumn,
		)
		if err != nil {
			return nil, err
		}
		var l, r ColumnBase
		l.Schema = schemaName
		l.Table = fkItem.table
		l.Name = fkItem.column
		r.Schema = l.Schema
		r.Table = fkItem.refTable
		r.Name = fkItem.refColumn
		if fkItem.fkID != prevFk {
			if cur != nil {
				retVal = append(retVal, cur)
			}
			cur = new(ForeignKey)
		}
		*cur = append(*cur, [2]*ColumnBase{&l, &r})
		prevFk = fkItem.fkID
	}

	if cur != nil {
		retVal = append(retVal, cur)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return retVal, nil
}
