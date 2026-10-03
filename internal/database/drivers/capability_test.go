package drivers

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
)

func TestForeignRepositoriesDoNotImplementCapabilities(t *testing.T) {
	// Capabilities and the database-switch guard stay InterBase-only; a foreign
	// repository that accidentally satisfied one would take a branch it cannot serve.
	repositories := map[dialect.DatabaseDriver]database.DBRepository{
		dialect.DatabaseDriverMySQL:      NewMySQLDBRepository(nil),
		dialect.DatabaseDriverPostgreSQL: NewPostgreSQLDBRepository(nil),
		dialect.DatabaseDriverSQLite3:    NewSQLite3DBRepository(nil),
		dialect.DatabaseDriverMssql:      NewMssqlDBRepository(nil),
		dialect.DatabaseDriverH2:         NewH2DBRepository(nil),
		dialect.DatabaseDriverVertica:    NewVerticaDBRepository(nil),
		dialect.DatabaseDriverClickhouse: NewClickhouseRepository(nil),
		dialect.DatabaseDriverOracle:     NewOracleDBRepository(nil),
	}

	for driver, repository := range repositories {
		t.Run(string(driver), func(t *testing.T) {
			if _, ok := repository.(database.CatalogRepository); ok {
				t.Errorf("%s must not implement CatalogRepository", driver)
			}
			if _, ok := repository.(database.DDLRepository); ok {
				t.Errorf("%s must not implement DDLRepository", driver)
			}
			if _, ok := repository.(database.ExplainRepository); ok {
				t.Errorf("%s must not implement ExplainRepository", driver)
			}
			if _, ok := repository.(database.CatalogSnapshotRepository); ok {
				t.Errorf("%s must not implement CatalogSnapshotRepository", driver)
			}
			if _, ok := repository.(database.DatabaseSwitchRepository); ok {
				t.Errorf("%s must not implement DatabaseSwitchRepository", driver)
			}
		})
	}
}
