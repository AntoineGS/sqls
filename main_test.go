package main

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
)

func TestServerRegistersEveryDriver(t *testing.T) {
	for _, driver := range []dialect.DatabaseDriver{
		dialect.DatabaseDriverMySQL,
		dialect.DatabaseDriverMySQL8,
		dialect.DatabaseDriverMySQL57,
		dialect.DatabaseDriverMySQL56,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
		dialect.DatabaseDriverMssql,
		dialect.DatabaseDriverOracle,
		dialect.DatabaseDriverH2,
		dialect.DatabaseDriverVertica,
		dialect.DatabaseDriverClickhouse,
		dialect.DatabaseDriverInterBase,
	} {
		if !database.Registered(driver) {
			t.Errorf("the sqls server does not register driver %q", driver)
		}
	}
}
