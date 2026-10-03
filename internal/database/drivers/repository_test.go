package drivers

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
)

func TestCreateRepositoryFromConnectionFallsBackToFactory(t *testing.T) {
	// Drivers that register no ConnFactory fall back to the *sql.DB factory and
	// stay untouched by this change.
	//
	// Every case here leaves DBConnection.Driver EMPTY on purpose. That is what
	// the real openers produce: postgreSQLOpen (postgresql.go:56), sqlite3Open
	// (sqlite3.go:25) and the "mock" opener (../database_mock.go:549) all return a
	// DBConnection with no Driver set. A lookup keyed off conn.Driver passes a
	// hand-built {Driver: postgresql} literal and fails every real connection,
	// so constructing one here would make this test agree with the bug.
	for _, driver := range []dialect.DatabaseDriver{
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
		dialect.DatabaseDriverMySQL,
	} {
		t.Run(string(driver), func(t *testing.T) {
			conn := &database.DBConnection{}
			if conn.Driver != "" {
				t.Fatalf("this test is only meaningful with an empty conn.Driver, got %q", conn.Driver)
			}
			repo, err := database.CreateRepositoryFromConnection(driver, conn)
			if err != nil {
				t.Fatalf("CreateRepositoryFromConnection(%q) error = %v", driver, err)
			}
			if repo == nil {
				t.Fatal("CreateRepositoryFromConnection() returned a nil repository")
			}
			if got := repo.Driver(); got != driver {
				t.Errorf("repository driver = %q, want %q", got, driver)
			}
		})
	}
}
