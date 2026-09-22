package database

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestInterBaseConfigBuildsLocalAndRemoteAttachmentsWithoutSecrets(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *DBConfig
		wantDSN     string
		wantCharset string
	}{
		{
			name: "explicit attachment and charset parameter",
			cfg: &DBConfig{
				Driver:         dialect.DatabaseDriverInterBase,
				DataSourceName: "server/3050:/srv/data/example.ib",
				User:           "alice",
				Passwd:         "do-not-log",
				Params:         map[string]string{"charset": "win1250"},
			},
			wantDSN:     "server/3050:/srv/data/example.ib",
			wantCharset: "WIN1250",
		},
		{
			name: "local path",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/var/lib/interbase/example.ib",
				User:   "alice",
			},
			wantDSN:     "/var/lib/interbase/example.ib",
			wantCharset: "UTF8",
		},
		{
			name: "remote defaults port",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Path:   "/var/lib/interbase/example.ib",
				User:   "alice",
			},
			wantDSN:     "db.example.test/3050:/var/lib/interbase/example.ib",
			wantCharset: "UTF8",
		},
		{
			name: "remote explicit port and db name fallback",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Port:   3307,
				DBName: "example.ib",
				User:   "alice",
			},
			wantDSN:     "db.example.test/3307:example.ib",
			wantCharset: "UTF8",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
			gotDSN, err := interBaseAttachment(test.cfg)
			if err != nil {
				t.Fatalf("interBaseAttachment() error = %v", err)
			}
			if gotDSN != test.wantDSN {
				t.Errorf("interBaseAttachment() = %q, want %q", gotDSN, test.wantDSN)
			}
			gotCharset, err := interBaseCharset(test.cfg)
			if err != nil {
				t.Fatalf("interBaseCharset() error = %v", err)
			}
			if gotCharset != test.wantCharset {
				t.Errorf("interBaseCharset() = %q, want %q", gotCharset, test.wantCharset)
			}
		})
	}
}

func TestInterBaseConfigRejectsUnsupportedConnectionModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  DBConfig
		want string
	}{
		{
			name: "missing user",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib"},
			want: "user",
		},
		{
			name: "missing attachment",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, User: "alice"},
			want: "dataSourceName",
		},
		{
			name: "unsupported protocol",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Proto: ProtoUDP, Path: "/tmp/example.ib", User: "alice"},
			want: "proto",
		},
		{
			name: "tcp requires host without explicit attachment",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Proto: ProtoTCP, Path: "/tmp/example.ib", User: "alice"},
			want: "host",
		},
		{
			name: "port range",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Host: "db.example.test", Port: 65536, Path: "/tmp/example.ib", User: "alice"},
			want: "port",
		},
		{
			name: "explicit attachment still rejects unsupported protocol",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, DataSourceName: "example.ib", Proto: ProtoUDP, User: "alice"},
			want: "proto",
		},
		{
			name: "ssh is explicit unsupported",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib", User: "alice", SSHCfg: &SSHConfig{}},
			want: "SSH",
		},
		{
			name: "unsupported charset",
			cfg:  DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib", User: "alice", Passwd: "secret", Params: map[string]string{"charset": "latin1"}},
			want: "charset",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if err == nil {
				t.Fatal("DBConfig.Validate() returned nil error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("DBConfig.Validate() error = %q, want mention %q", err, test.want)
			}
			if strings.Contains(err.Error(), test.cfg.Passwd) && test.cfg.Passwd != "" {
				t.Fatalf("DBConfig.Validate() leaked password: %q", err)
			}
		})
	}
}

func TestInterBaseDriverIsRegistered(t *testing.T) {
	if !Registered(dialect.DatabaseDriverInterBase) {
		t.Fatalf("InterBase driver is not registered")
	}
	if _, err := CreateRepository(dialect.DatabaseDriverInterBase, nil); err != nil {
		t.Fatalf("CreateRepository() error = %v", err)
	}
}

func TestInterBaseOpenRejectsNilConfig(t *testing.T) {
	if _, err := Open(nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "config") {
		t.Fatalf("Open(nil) error = %v, want a configuration error", err)
	}
}

func interBaseFixed(value string) string {
	return value + strings.Repeat(" ", 31-len(value))
}

func TestDBConnectionDriverVariant(t *testing.T) {
	var nilConn *DBConnection
	if got, want := nilConn.DriverVariant(), (dialect.DriverVariant{}); got != want {
		t.Errorf("(*DBConnection)(nil).DriverVariant() = %#v, want %#v", got, want)
	}

	conn := &DBConnection{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	}
	want := dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	}
	if got := conn.DriverVariant(); got != want {
		t.Errorf("DriverVariant() = %#v, want %#v", got, want)
	}

	noVariant := &DBConnection{Driver: dialect.DatabaseDriverPostgreSQL}
	if got, want := noVariant.DriverVariant().Variant, dialect.SQLVariantDefault; got != want {
		t.Errorf("a driver with no variants must report %q, got %q", want, got)
	}
}

func TestCreateRepositoryFromConnectionPrefersConnFactory(t *testing.T) {
	conn := &DBConnection{
		Driver:       dialect.DatabaseDriverInterBase,
		Variant:      dialect.SQLVariantInterBase1,
		DatabaseName: "db.example.test/3050:/srv/interbase/example.ib",
	}

	repo, err := CreateRepositoryFromConnection(dialect.DatabaseDriverInterBase, conn)
	if err != nil {
		t.Fatalf("CreateRepositoryFromConnection() error = %v", err)
	}
	ib, ok := repo.(*InterBaseDBRepository)
	if !ok {
		t.Fatalf("CreateRepositoryFromConnection() = %T, want *InterBaseDBRepository", repo)
	}
	if got, want := ib.SQLDialect, 1; got != want {
		t.Errorf("repository SQLDialect = %d, want %d", got, want)
	}
	if got, want := ib.DatabaseName, conn.DatabaseName; got != want {
		t.Errorf("repository DatabaseName = %q, want %q", got, want)
	}
}

func TestCreateRepositoryFromConnectionFallsBackToFactory(t *testing.T) {
	// Drivers that register no ConnFactory fall back to the *sql.DB factory and
	// stay untouched by this change.
	//
	// Every case here leaves DBConnection.Driver EMPTY on purpose. That is what
	// the real openers produce: openPostgreSQL (postgresql.go:55), openSQLite3
	// (sqlite3.go:24) and the "mock" opener (database_mock.go:549) all return a
	// DBConnection with no Driver set. A lookup keyed off conn.Driver passes a
	// hand-built {Driver: postgresql} literal and fails every real connection,
	// so constructing one here would make this test agree with the bug.
	for _, driver := range []dialect.DatabaseDriver{
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
		dialect.DatabaseDriverMySQL,
	} {
		t.Run(string(driver), func(t *testing.T) {
			conn := &DBConnection{}
			if conn.Driver != "" {
				t.Fatalf("this test is only meaningful with an empty conn.Driver, got %q", conn.Driver)
			}
			repo, err := CreateRepositoryFromConnection(driver, conn)
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

func TestCreateRepositoryFromConnectionRejectsNil(t *testing.T) {
	if _, err := CreateRepositoryFromConnection(dialect.DatabaseDriverPostgreSQL, nil); err == nil {
		t.Fatal("CreateRepositoryFromConnection(nil) returned a nil error")
	}
	if _, err := CreateRepositoryFromConnection("nope", &DBConnection{}); err == nil {
		t.Fatal("CreateRepositoryFromConnection() with an unknown driver returned a nil error")
	}
}

func TestInterBaseRepositoryDefaultsToDialect3(t *testing.T) {
	// The *sql.DB factory has no connection to read, so it leaves the zero
	// value, which means Dialect 3 exactly as it does for interbase.Config.
	repo, err := CreateRepository(dialect.DatabaseDriverInterBase, nil)
	if err != nil {
		t.Fatal(err)
	}
	ib, ok := repo.(*InterBaseDBRepository)
	if !ok {
		t.Fatalf("CreateRepository() = %T, want *InterBaseDBRepository", repo)
	}
	if got, want := ib.SQLDialect, 0; got != want {
		t.Errorf("repository SQLDialect = %d, want %d (zero means dialect 3)", got, want)
	}
	if got, want := ib.DatabaseName, ""; got != want {
		t.Errorf("repository DatabaseName = %q, want %q", got, want)
	}
}

func TestInterBaseConfigValidatesDialect(t *testing.T) {
	accepted := []int{0, 1, 3}
	for _, sqlDialect := range accepted {
		cfg := DBConfig{
			Driver:  dialect.DatabaseDriverInterBase,
			Path:    "/tmp/example.ib",
			User:    "alice",
			Dialect: sqlDialect,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("dialect %d: Validate() error = %v, want nil", sqlDialect, err)
		}
	}

	rejected := []int{2, 4, -1, 100}
	for _, sqlDialect := range rejected {
		cfg := DBConfig{
			Driver:  dialect.DatabaseDriverInterBase,
			Path:    "/tmp/example.ib",
			User:    "alice",
			Passwd:  "secret",
			Dialect: sqlDialect,
		}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("dialect %d: Validate() returned a nil error", sqlDialect)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "dialect") {
			t.Errorf("dialect %d: Validate() error = %q, want it to mention dialect", sqlDialect, err)
		}
		if strings.Contains(err.Error(), cfg.Passwd) {
			t.Errorf("dialect %d: Validate() leaked the password: %q", sqlDialect, err)
		}
	}
}

func TestDialectIsRejectedForNonInterBaseDrivers(t *testing.T) {
	drivers := []dialect.DatabaseDriver{
		dialect.DatabaseDriverMySQL,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
	}
	for _, driver := range drivers {
		cfg := DBConfig{
			Driver:         driver,
			DataSourceName: "whatever",
			Proto:          ProtoTCP,
			Host:           "localhost",
			User:           "alice",
			Dialect:        3,
		}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate() with a dialect returned a nil error", driver)
			continue
		}
		message := strings.ToLower(err.Error())
		if !strings.Contains(message, "dialect") || !strings.Contains(message, "interbase") {
			t.Errorf("%s: Validate() error = %q, want it to mention dialect and interbase", driver, err)
		}
	}

	// Zero is the default and must stay silent for every driver.
	for _, driver := range drivers {
		cfg := DBConfig{Driver: driver, DataSourceName: "whatever"}
		if err := cfg.Validate(); err != nil && strings.Contains(strings.ToLower(err.Error()), "dialect") {
			t.Errorf("%s: a zero dialect must not be rejected: %v", driver, err)
		}
	}
}
