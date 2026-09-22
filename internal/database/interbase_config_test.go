package database

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"gopkg.in/yaml.v2"
)

func TestInterBaseConfigValidatesCharset(t *testing.T) {
	accepted := []struct {
		param string
		want  string
	}{
		{param: "", want: "UTF8"},
		{param: "UTF8", want: "UTF8"},
		{param: "utf8", want: "UTF8"},
		{param: "WIN1250", want: "WIN1250"},
		{param: "win1250", want: "WIN1250"},
		{param: "WIN1252", want: "WIN1252"},
		{param: "win1252", want: "WIN1252"},
		{param: "ISO8859_1", want: "ISO8859_1"},
		{param: "iso8859_1", want: "ISO8859_1"},
		{param: "ASCII", want: "ASCII"},
		{param: "ascii", want: "ASCII"},
		{param: "  utf8  ", want: "UTF8"},
	}
	for _, test := range accepted {
		t.Run("accepts "+test.param, func(t *testing.T) {
			cfg := &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/tmp/example.ib",
				User:   "alice",
			}
			if test.param != "" {
				cfg.Params = map[string]string{"charset": test.param}
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
			got, err := interBaseCharset(cfg)
			if err != nil || got != test.want {
				t.Fatalf("interBaseCharset(%q) = (%q, %v), want (%q, nil)", test.param, got, err, test.want)
			}
		})
	}

	rejected := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{name: "unsupported value", params: map[string]string{"charset": "latin1"}, want: "charset"},
		{name: "hyphenated spelling is not the catalog name", params: map[string]string{"charset": "UTF-8"}, want: "charset"},
		{name: "conflicting duplicates", params: map[string]string{"charset": "UTF8", "CHARSET": "ASCII"}, want: "conflicting"},
	}
	for _, test := range rejected {
		t.Run("rejects "+test.name, func(t *testing.T) {
			cfg := &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/tmp/example.ib",
				User:   "alice",
				Passwd: "do-not-log",
				Params: test.params,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("DBConfig.Validate() returned nil error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("DBConfig.Validate() error = %q, want mention %q", err, test.want)
			}
			if strings.Contains(err.Error(), cfg.Passwd) {
				t.Fatalf("DBConfig.Validate() leaked password: %q", err)
			}
		})
	}
}

func TestInterBaseCharsetMatchesDriverAllowlist(t *testing.T) {
	// interbase-go normalizeCharset (interbase.go:383-392) accepts exactly these
	// five names, and the empty value defaults to UTF8. That normalizer is
	// unexported and only builds with cgo, so sqls keeps this copy for untagged
	// validation. The tagged TestInterBaseCharsetAllowlistMatchesDriverNormalizer
	// proves the two have not drifted.
	want := []string{"UTF8", "WIN1250", "WIN1252", "ISO8859_1", "ASCII"}
	if !reflect.DeepEqual(interBaseCharsets, want) {
		t.Fatalf("interBaseCharsets = %#v, want %#v", interBaseCharsets, want)
	}
	if got, err := interBaseCharset(&DBConfig{}); err != nil || got != "UTF8" {
		t.Fatalf("interBaseCharset(empty) = (%q, %v), want (\"UTF8\", nil)", got, err)
	}
}

func TestInterBaseConfigValidatesConnectionOptions(t *testing.T) {
	const passphrase = "phrase-do-not-log"
	base := func() *DBConfig {
		return &DBConfig{
			Driver: dialect.DatabaseDriverInterBase,
			Host:   "db.example.test",
			Path:   "/srv/interbase/example.ib",
			User:   "alice",
			Passwd: "do-not-log",
		}
	}

	rejected := []struct {
		name   string
		mutate func(*DBConfig)
		want   string
	}{
		{
			name:   "role longer than the driver's 255-byte limit",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{Role: strings.Repeat("R", 256)} },
			want:   "role",
		},
		{
			name:   "role containing a NUL byte",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{Role: "SQLS\x00READONLY"} },
			want:   "role",
		},
		{
			name:   "connectTimeout is not a Go duration",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{ConnectTimeout: "abc"} },
			want:   "connecttimeout",
		},
		{
			name:   "negative connectTimeout",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{ConnectTimeout: "-1s"} },
			want:   "connecttimeout",
		},
		{
			name: "tls with a raw attachment string",
			mutate: func(c *DBConfig) {
				c.Host = ""
				c.Path = ""
				c.DataSourceName = "db.example.test/3050:/srv/interbase/example.ib"
				c.InterBase = &InterBaseConfig{TLS: &InterBaseTLSConfig{Enabled: true, ClientPassPhrase: passphrase}}
			},
			want: "tls",
		},
		{
			// The thinnest TLS request there is: Enabled alone, no certificate
			// options, no host. It needs its own case because it is the shape an
			// implementer is most likely to let slip past hasOptions. The driver
			// does reject it — TLSConfig.hasOptions counts Enabled itself
			// (interbase.go:32-36) — but only at NewConnector time and with the
			// message "TLS options require a host", which names no configuration
			// key. sqls must reject it in Validate, naming connections[].host.
			name: "tls enabled with no other option and no host",
			mutate: func(c *DBConfig) {
				c.Host = ""
				c.InterBase = &InterBaseConfig{TLS: &InterBaseTLSConfig{Enabled: true}}
			},
			want: "host",
		},
		{
			name: "tls options without enabled",
			mutate: func(c *DBConfig) {
				c.InterBase = &InterBaseConfig{TLS: &InterBaseTLSConfig{
					ServerPublicFile: "/etc/interbase/server.pem",
					ClientPassPhrase: passphrase,
				}}
			},
			want: "enabled",
		},
		{
			name: "interbase block on another driver",
			mutate: func(c *DBConfig) {
				c.Driver = dialect.DatabaseDriverMySQL
				c.Proto = ProtoTCP
				c.InterBase = &InterBaseConfig{Role: "SQLS_READONLY"}
			},
			want: "interbase",
		},
	}
	for _, test := range rejected {
		t.Run("rejects "+test.name, func(t *testing.T) {
			cfg := base()
			test.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("DBConfig.Validate() returned nil error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("DBConfig.Validate() error = %q, want mention %q", err, test.want)
			}
			if strings.Contains(err.Error(), cfg.Passwd) {
				t.Fatalf("DBConfig.Validate() leaked the password: %q", err)
			}
			if strings.Contains(err.Error(), passphrase) {
				t.Fatalf("DBConfig.Validate() leaked the client passphrase: %q", err)
			}
		})
	}

	accepted := []struct {
		name   string
		mutate func(*DBConfig)
	}{
		{name: "no interbase block", mutate: func(*DBConfig) {}},
		{name: "empty interbase block", mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{} }},
		{
			name:   "role at the 255-byte limit",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{Role: strings.Repeat("R", 255)} },
		},
		{
			name:   "connectTimeout in seconds",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{ConnectTimeout: " 10s "} },
		},
		{
			name:   "zero connectTimeout keeps the client default",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{ConnectTimeout: "0s"} },
		},
		{
			name: "tls enabled with a host",
			mutate: func(c *DBConfig) {
				c.InterBase = &InterBaseConfig{TLS: &InterBaseTLSConfig{
					Enabled:              true,
					ServerPublicFile:     "/etc/interbase/server.pem",
					ClientPassPhraseFile: "/etc/interbase/client.pass",
				}}
			},
		},
		{
			name:   "tls block present but empty",
			mutate: func(c *DBConfig) { c.InterBase = &InterBaseConfig{TLS: &InterBaseTLSConfig{}} },
		},
	}
	for _, test := range accepted {
		t.Run("accepts "+test.name, func(t *testing.T) {
			cfg := base()
			test.mutate(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
		})
	}
}

func TestInterBaseConfigStillRejectsSSHTunnelling(t *testing.T) {
	cfg := &DBConfig{
		Driver:    dialect.DatabaseDriverInterBase,
		Path:      "/srv/interbase/example.ib",
		User:      "alice",
		SSHCfg:    &SSHConfig{Host: "bastion", User: "alice", PrivateKey: "/dev/null"},
		InterBase: &InterBaseConfig{Role: "SQLS_READONLY"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Fatalf("DBConfig.Validate() error = %v, want the unsupported-SSH refusal", err)
	}
}

func TestInterBaseConnectionOptionsUnmarshalFromYAML(t *testing.T) {
	const document = `
driver: interbase
host: db.example.test
port: 3050
path: /srv/interbase/centrale.ib
user: sqls_reader
passwd: "your-password"
params:
  charset: WIN1252
interbase:
  role: SQLS_READONLY
  connectTimeout: 10s
  tls:
    enabled: true
    serverPublicFile: /etc/interbase/server.pem
    clientPassPhraseFile: /etc/interbase/client.pass
`
	var cfg DBConfig
	if err := yaml.Unmarshal([]byte(document), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
	if cfg.InterBase == nil {
		t.Fatal("yaml.Unmarshal() left DBConfig.InterBase nil; check the yaml tag")
	}
	if cfg.InterBase.Role != "SQLS_READONLY" {
		t.Errorf("role = %q, want SQLS_READONLY", cfg.InterBase.Role)
	}
	if cfg.InterBase.ConnectTimeout != "10s" {
		t.Errorf("connectTimeout = %q, want 10s", cfg.InterBase.ConnectTimeout)
	}
	if cfg.InterBase.TLS == nil {
		t.Fatal("tls block did not unmarshal")
	}
	want := InterBaseTLSConfig{
		Enabled:              true,
		ServerPublicFile:     "/etc/interbase/server.pem",
		ClientPassPhraseFile: "/etc/interbase/client.pass",
	}
	if !reflect.DeepEqual(*cfg.InterBase.TLS, want) {
		t.Errorf("tls = %#v, want %#v", *cfg.InterBase.TLS, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DBConfig.Validate() error = %v", err)
	}
	timeout, err := interBaseConnectTimeout(&cfg)
	if err != nil || timeout != 10*time.Second {
		t.Fatalf("interBaseConnectTimeout() = (%v, %v), want (10s, nil)", timeout, err)
	}
}

func TestInterBaseDriverConfigMapping(t *testing.T) {
	tests := []struct {
		name string
		cfg  *DBConfig
		want interBaseConnConfig
	}{
		{
			name: "local path attaches without a host",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Path:   "/var/lib/interbase/example.ib",
				User:   "alice",
				Passwd: "secret",
			},
			want: interBaseConnConfig{
				Database: "/var/lib/interbase/example.ib",
				User:     "alice",
				Password: "secret",
				Charset:  "UTF8",
			},
		},
		{
			name: "host and explicit port",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Port:   3307,
				Path:   "/srv/interbase/example.ib",
				User:   "alice",
			},
			want: interBaseConnConfig{
				Host:     "db.example.test/3307",
				Database: "/srv/interbase/example.ib",
				User:     "alice",
				Charset:  "UTF8",
			},
		},
		{
			name: "host defaults the port and falls back to dbName",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				DBName: "example.ib",
				User:   "alice",
			},
			want: interBaseConnConfig{
				Host:     "db.example.test/3050",
				Database: "example.ib",
				User:     "alice",
				Charset:  "UTF8",
			},
		},
		{
			name: "dataSourceName stays a raw attachment with no host",
			cfg: &DBConfig{
				Driver:         dialect.DatabaseDriverInterBase,
				DataSourceName: "db.example.test/3050:/srv/interbase/example.ib",
				User:           "alice",
				Params:         map[string]string{"charset": "ascii"},
			},
			want: interBaseConnConfig{
				Database: "db.example.test/3050:/srv/interbase/example.ib",
				User:     "alice",
				Charset:  "ASCII",
			},
		},
		{
			name: "role, timeout and tls reach the driver",
			cfg: &DBConfig{
				Driver: dialect.DatabaseDriverInterBase,
				Host:   "db.example.test",
				Path:   "/srv/interbase/example.ib",
				User:   "alice",
				InterBase: &InterBaseConfig{
					Role:           "SQLS_READONLY",
					ConnectTimeout: "10s",
					TLS: &InterBaseTLSConfig{
						Enabled:              true,
						ServerPublicFile:     "/etc/interbase/server.pem",
						ClientPassPhraseFile: "/etc/interbase/client.pass",
					},
				},
			},
			want: interBaseConnConfig{
				Host:           "db.example.test/3050",
				Database:       "/srv/interbase/example.ib",
				User:           "alice",
				Role:           "SQLS_READONLY",
				Charset:        "UTF8",
				ConnectTimeout: 10 * time.Second,
				TLS: interBaseTLSSettings{
					Enabled:              true,
					ServerPublicFile:     "/etc/interbase/server.pem",
					ClientPassPhraseFile: "/etc/interbase/client.pass",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err != nil {
				t.Fatalf("DBConfig.Validate() error = %v", err)
			}
			got, err := interBaseConnectionConfig(test.cfg)
			if err != nil {
				t.Fatalf("interBaseConnectionConfig() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("interBaseConnectionConfig() = %#v, want %#v", got, test.want)
			}

			// Without TLS the driver composes Host + ":" + Database
			// (interbase.go:209-235), which must reproduce the attachment sqls
			// built by hand and still displays. This is the behavior-preservation
			// guard for moving to structured host/database.
			attachment, err := interBaseAttachment(test.cfg)
			if err != nil {
				t.Fatalf("interBaseAttachment() error = %v", err)
			}
			composed := got.Database
			if got.Host != "" {
				composed = got.Host + ":" + got.Database
			}
			if composed != attachment {
				t.Errorf("structured mapping composes %q, want the display attachment %q", composed, attachment)
			}
		})
	}
}

func TestInterBaseCurrentDatabaseAndDatabases(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	ctx := context.Background()

	named := &InterBaseDBRepository{DatabaseName: attachment}
	if got, err := named.CurrentDatabase(ctx); err != nil || got != attachment {
		t.Fatalf("CurrentDatabase() = (%q, %v), want (%q, nil)", got, err, attachment)
	}
	if got, err := named.Databases(ctx); err != nil || !reflect.DeepEqual(got, []string{attachment}) {
		t.Fatalf("Databases() = (%#v, %v), want the single attachment", got, err)
	}

	// Repositories built from a bare *sql.DB have no identity to report; the
	// existing assertions in interbase_test.go:18-23 depend on this branch.
	anonymous := &InterBaseDBRepository{}
	if got, err := anonymous.CurrentDatabase(ctx); err != nil || got != "" {
		t.Fatalf("CurrentDatabase() = (%q, %v), want (empty, nil)", got, err)
	}
	if got, err := anonymous.Databases(ctx); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("Databases() = (%#v, %v), want an empty list", got, err)
	}
}

func TestInterBaseConnectionConfigRejectsInvalidSettings(t *testing.T) {
	if _, err := interBaseConnectionConfig(nil); err == nil {
		t.Fatal("interBaseConnectionConfig(nil) returned nil error")
	}
	cfg := &DBConfig{
		Driver: dialect.DatabaseDriverInterBase,
		Path:   "/srv/interbase/example.ib",
		User:   "alice",
		InterBase: &InterBaseConfig{
			TLS: &InterBaseTLSConfig{Enabled: true},
		},
	}
	if _, err := interBaseConnectionConfig(cfg); err == nil {
		t.Fatal("interBaseConnectionConfig() accepted TLS without a host")
	}
}
