package fbmeta

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestNormalizeTargetCanonicalizesSchemeAndPreservesAttachment(t *testing.T) {
	want := metadata.Key{Engine: "firebird", URL: `fb://server/3051:C:\Data\db.fdb`}
	for _, target := range []string{`server/3051:C:\Data\db.fdb`, `FB://server/3051:C:\Data\db.fdb`} {
		got, err := NormalizeTarget(target)
		if err != nil {
			t.Fatalf("NormalizeTarget(%q): %v", target, err)
		}
		if got != want {
			t.Errorf("NormalizeTarget(%q) = %+v, want %+v", target, got, want)
		}
	}
}

func TestNormalizeTargetRejectsBlank(t *testing.T) {
	for _, target := range []string{"", "  ", "fb://"} {
		if _, err := NormalizeTarget(target); !errors.Is(err, metadata.ErrInvalidTarget) {
			t.Errorf("NormalizeTarget(%q) error = %v, want ErrInvalidTarget", target, err)
		}
	}
}

func TestNormalizeTargetRejectsSurroundingWhitespace(t *testing.T) {
	for _, target := range []string{" server:/db.fdb", "server:/db.fdb ", " fb://server:/db.fdb"} {
		if _, err := NormalizeTarget(target); !errors.Is(err, metadata.ErrInvalidTarget) {
			t.Errorf("NormalizeTarget(%q) error = %v, want ErrInvalidTarget", target, err)
		}
	}
}

func TestAttachment(t *testing.T) {
	cases := []struct {
		in, host string
		port     int
		path     string
	}{
		{`server:C:\data\db.fdb`, "server", 3050, `C:\data\db.fdb`},
		{`server/3051:/srv/db.fdb`, "server", 3051, "/srv/db.fdb"},
		{`10.0.0.5:employee`, "10.0.0.5", 3050, "employee"},
		{`C:\data\db.fdb`, "localhost", 3050, `C:\data\db.fdb`},
		{`/srv/db.fdb`, "localhost", 3050, "/srv/db.fdb"},
		{`fb://server:/srv/db.fdb`, "server", 3050, "/srv/db.fdb"},
	}
	for _, c := range cases {
		host, port, path, err := Attachment(c.in)
		if err != nil || host != c.host || port != c.port || path != c.path {
			t.Errorf("%q = (%q, %d, %q, %v), want (%q, %d, %q)", c.in, host, port, path, err, c.host, c.port, c.path)
		}
	}
	for _, bad := range []string{"", "  ", "server/x:/db", "server/70000:/db", "server:"} {
		if _, _, _, err := Attachment(bad); !errors.Is(err, metadata.ErrInvalidTarget) {
			t.Errorf("Attachment(%q) error = %v, want ErrInvalidTarget", bad, err)
		}
	}
}

func TestAttachmentRejectsSurroundingWhitespace(t *testing.T) {
	for _, target := range []string{" server:/db.fdb", "server:/db.fdb ", " fb://server:/db.fdb"} {
		if _, _, _, err := Attachment(target); !errors.Is(err, metadata.ErrInvalidTarget) {
			t.Errorf("Attachment(%q) error = %v, want ErrInvalidTarget", target, err)
		}
	}
}

func TestDriverDSNRoundTripsSecretsAndPaths(t *testing.T) {
	dsn, err := DriverDSN(`server/3051:C:\data\db.fdb`, "sysdba", "p@ss:w/r%d?", "RDB$ADMIN", 1, url.Values{"none_text_fallback": {"WIN1252"}})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Scheme != "firebird" || u.User.Username() != "sysdba" || pw != "p@ss:w/r%d?" || u.Host != "server:3051" {
		t.Fatalf("dsn = %s", dsn)
	}
	if strings.TrimPrefix(u.Path, "/") != `C:\data\db.fdb` {
		t.Fatalf("path = %q", u.Path)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"charset": "NONE", "explicit_tx_only": "true", "sql_type_names": "true",
		"auth_plugin_name": "Legacy_Auth", "auth_plugin_list": "Legacy_Auth",
		"wire_crypt": "false", "sql_dialect": "1", "role": "RDB$ADMIN",
		"none_text_fallback": "WIN1252",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
}

func TestDriverDSNDefaultsDialectAndKeepsUnixRoot(t *testing.T) {
	dsn, err := DriverDSN("/srv/db.fdb", "sysdba", "masterkey", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/srv/db.fdb" {
		t.Fatalf("path = %q, want absolute /srv/db.fdb", u.Path)
	}
	if got := u.Query().Get("sql_dialect"); got != "3" {
		t.Errorf("sql_dialect = %q, want 3", got)
	}
}

func TestDriverDSNRejectsReservedExtraOptions(t *testing.T) {
	reserved := []string{
		"auth_plugin_name", "auth_plugin_list", "charset", "CHARSET", "explicit_tx_only",
		"sql_type_names", "wire_crypt", "sql_dialect", "role",
	}
	for _, key := range reserved {
		t.Run(key, func(t *testing.T) {
			if _, err := DriverDSN("server:/db.fdb", "SYSDBA", "secret", "READER", 3, url.Values{key: {"override"}}); err == nil {
				t.Fatalf("DriverDSN accepted reserved extra option %q", key)
			}
		})
	}
}

func TestDriverDSNPathMatchesDriverParser(t *testing.T) {
	cases := []struct {
		name, target, want, parserWouldSend string
		wantErr                             bool
	}{
		{"Windows absolute", `server:C:\data\db.fdb`, `C:\data\db.fdb`, `C:\data\db.fdb`, false},
		{"Unix multi-segment absolute", "/srv/db.fdb", "/srv/db.fdb", "/srv/db.fdb", false},
		{"Firebird fixture absolute", "/firebird/data/fixture.fdb", "/firebird/data/fixture.fdb", "/firebird/data/fixture.fdb", false},
		{"database alias", "server:employee", "employee", "employee", false},
		{"Unix root single segment", "/db.fdb", "", "db.fdb", true},
		{"Unix path containing colon", "/srv/a:b.fdb", "", "srv/a:b.fdb", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn, err := DriverDSN(c.target, "SYSDBA", "secret", "", 3, nil)
			if c.wantErr {
				if err == nil {
					t.Fatalf("DriverDSN(%q) = %q, want an unsupported-path error", c.target, dsn)
				}
				if got := driverParserDBName(rawPathDSN(c.target)); got != c.parserWouldSend {
					t.Fatalf("driver parser would send %q, want %q", got, c.parserWouldSend)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := driverParserDBName(dsn); got != c.want || got != c.parserWouldSend {
				t.Fatalf("driver database name = %q, want %q (DSN %q)", got, c.want, dsn)
			}
		})
	}
}

// driverParserDBName documents and mirrors the driver's path transformation
// in firebirdsql/dsn.go:73-80, since parseDSN is unexported and fbmeta
// deliberately has no driver dependency.
func driverParserDBName(dsn string) string {
	u, _ := url.Parse(dsn)
	dbName := u.Path
	if len(dbName) > 1 && !strings.ContainsRune(dbName[1:], '/') {
		dbName = dbName[1:]
	}
	if len(dbName) > 2 && strings.ContainsRune(dbName[2:], ':') {
		dbName = dbName[1:]
	}
	return dbName
}

func rawPathDSN(path string) string {
	u := url.URL{Scheme: "firebird", User: url.UserPassword("SYSDBA", "secret"), Host: "server:3050", Path: path}
	return u.String()
}
