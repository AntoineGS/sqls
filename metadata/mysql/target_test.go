package mymeta

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sqls-server/sqls/metadata"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		want     Target
		wantAddr string
		wantErr  bool
		secret   string
	}{
		{
			name:     "explicit user and port",
			target:   "alice@DB.Example:3307/mde_fixture",
			want:     Target{User: "alice", Host: "DB.Example", Port: 3307, Schema: "mde_fixture"},
			wantAddr: "DB.Example:3307",
		},
		{
			name:     "default port",
			target:   "DB.Example/mde_fixture",
			want:     Target{Host: "DB.Example", Port: 3306, Schema: "mde_fixture"},
			wantAddr: "DB.Example:3306",
		},
		{name: "blank host", target: "alice@/mde_fixture", wantErr: true},
		{name: "blank schema", target: "alice@DB.Example/", wantErr: true},
		{name: "zero port", target: "DB.Example:0/mde_fixture", wantErr: true},
		{name: "port too large", target: "DB.Example:65536/mde_fixture", wantErr: true},
		{name: "non-numeric port", target: "DB.Example:port/mde_fixture", wantErr: true},
		{name: "password in authority", target: "alice:secret@DB.Example/mde_fixture", wantErr: true, secret: "secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTarget(tt.target)
			if tt.wantErr {
				if !errors.Is(err, metadata.ErrInvalidTarget) {
					t.Fatalf("ParseTarget(%q) error = %v, want ErrInvalidTarget", tt.target, err)
				}
				if tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
					t.Fatalf("ParseTarget error leaked password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.target, err)
			}
			if got != tt.want {
				t.Fatalf("ParseTarget(%q) = %+v, want %+v", tt.target, got, tt.want)
			}
			if got.Addr() != tt.wantAddr {
				t.Fatalf("Addr() = %q, want %s", got.Addr(), tt.wantAddr)
			}
		})
	}
}

func TestKeyFor(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		want    metadata.Key
		wantErr bool
	}{
		{
			name:   "explicit port",
			target: "alice@DB.Example:3307/mde_fixture",
			want:   metadata.Key{Engine: "mysql", URL: "mysql://db.example:3307/mde_fixture"},
		},
		{
			name:   "default port",
			target: "alice@DB.Example/mde_fixture",
			want:   metadata.Key{Engine: "mysql", URL: "mysql://db.example:3306/mde_fixture"},
		},
		{name: "invalid target", target: "alice:secret@DB.Example/mde_fixture", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := KeyFor(Config{Target: tt.target})
			if tt.wantErr {
				if !errors.Is(err, metadata.ErrInvalidTarget) {
					t.Fatalf("KeyFor() error = %v, want ErrInvalidTarget", err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatalf("KeyFor() error leaked password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("KeyFor(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("KeyFor() = %+v, want %+v", got, tt.want)
			}
		})
	}

	cfg := Config{Target: "alice@DB.Example/mde_fixture", User: " override ", Password: "secret"}
	key, err := KeyFor(cfg)
	if err != nil || key != (metadata.Key{Engine: "mysql", URL: "mysql://db.example:3306/mde_fixture"}) {
		t.Fatalf("KeyFor() = %+v, %v", key, err)
	}
	scope, err := ScopeFor(cfg)
	if err != nil || scope.Principal != "override" || scope.TextPolicy != "utf8mb4" {
		t.Fatalf("ScopeFor() = %+v, %v", scope, err)
	}
	if strings.Contains(key.URL+scope.Principal+scope.TextPolicy, "secret") {
		t.Fatalf("key or scope contains password: key=%+v scope=%+v", key, scope)
	}
}

func TestScopeForUsesEffectiveLogin(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantUser  string
		wantErrIs error
	}{
		{
			name:     "target authority user",
			cfg:      Config{Target: "alice@DB.Example/mde_fixture"},
			wantUser: "alice",
		},
		{
			name:     "trimmed config override",
			cfg:      Config{Target: "alice@DB.Example/mde_fixture", User: " bob "},
			wantUser: "bob",
		},
		{
			name:      "blank effective login",
			cfg:       Config{Target: "DB.Example/mde_fixture", User: "  "},
			wantErrIs: metadata.ErrScopeRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ScopeFor(tt.cfg)
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("ScopeFor() error = %v, want %v", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ScopeFor(): %v", err)
			}
			if got.Principal != tt.wantUser || got.TextPolicy != "utf8mb4" {
				t.Fatalf("ScopeFor() = %+v, want principal %q and utf8mb4", got, tt.wantUser)
			}
		})
	}
}

func TestDriverDSNDefaultsAndTimeout(t *testing.T) {
	tests := []struct {
		name           string
		connectTimeout time.Duration
		extra          url.Values
		wantTimeout    time.Duration
	}{
		{name: "defaults", wantTimeout: 10 * time.Second},
		{name: "configured timeout", connectTimeout: 4 * time.Second, wantTimeout: 4 * time.Second},
		{name: "extra timeout override", connectTimeout: 4 * time.Second, extra: url.Values{"timeout": {"2s"}}, wantTimeout: 2 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, err := DriverDSN(Config{
				Target:         "alice@DB.Example:3307/mde_fixture",
				User:           " override ",
				Password:       "secret",
				ConnectTimeout: tt.connectTimeout,
			}, tt.extra)
			if err != nil {
				t.Fatalf("DriverDSN(): %v", err)
			}
			parsed, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("mysql.ParseDSN(%q): %v", dsn, err)
			}
			if parsed.User != "override" || parsed.Passwd != "secret" {
				t.Fatalf("parsed login = %q:%q, want override:secret", parsed.User, parsed.Passwd)
			}
			if parsed.Net != "tcp" || parsed.Addr != "DB.Example:3307" || parsed.DBName != "mde_fixture" {
				t.Fatalf("parsed endpoint = %s(%s)/%s, want tcp(DB.Example:3307)/mde_fixture", parsed.Net, parsed.Addr, parsed.DBName)
			}
			if parsed.TLSConfig != "preferred" || parsed.ParseTime || parsed.InterpolateParams || !parsed.ClientFoundRows {
				t.Fatalf("parsed options = tls:%q parseTime:%t interpolateParams:%t clientFoundRows:%t", parsed.TLSConfig, parsed.ParseTime, parsed.InterpolateParams, parsed.ClientFoundRows)
			}
			if parsed.Timeout != tt.wantTimeout {
				t.Fatalf("parsed timeout = %s, want %s", parsed.Timeout, tt.wantTimeout)
			}
			queryStart := strings.IndexByte(dsn, '?')
			if queryStart < 0 {
				t.Fatalf("DSN has no query options: %q", dsn)
			}
			query, err := url.ParseQuery(dsn[queryStart+1:])
			if err != nil {
				t.Fatalf("parse DSN query: %v", err)
			}
			if got := query.Get("charset"); got != "utf8mb4" {
				t.Fatalf("charset = %q, want utf8mb4", got)
			}
			if _, ok := query["allowPublicKeyRetrieval"]; ok {
				t.Fatalf("DSN unexpectedly contains allowPublicKeyRetrieval: %q", dsn)
			}
		})
	}
}

func TestDriverDSNUsesAuthorityUserUnlessOverridden(t *testing.T) {
	tests := []struct {
		name string
		user string
		want string
	}{
		{name: "authority user", want: "alice"},
		{name: "config user", user: " bob ", want: "bob"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, err := DriverDSN(Config{Target: "alice@DB.Example/mde_fixture", User: tt.user}, nil)
			if err != nil {
				t.Fatalf("DriverDSN(): %v", err)
			}
			parsed, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("mysql.ParseDSN(): %v", err)
			}
			if parsed.User != tt.want {
				t.Fatalf("parsed user = %q, want %q", parsed.User, tt.want)
			}
		})
	}
}

func TestDriverDSNUsernameRoundTripsWithMetadataScope(t *testing.T) {
	tests := []struct {
		name    string
		user    string
		wantErr bool
	}{
		{name: "plain username", user: "alice"},
		{name: "at sign username", user: "alice@admin"},
		{name: "slash username", user: "alice/admin"},
		{name: "colon username rejected", user: "alice:admin", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Target: "DB.Example/mde_fixture", User: tt.user, Password: "dsn-secret"}
			dsn, err := DriverDSN(cfg, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("DriverDSN() accepted a username that mysql.ParseDSN cannot represent")
				}
				if !strings.Contains(err.Error(), "username") || !strings.Contains(err.Error(), ":") {
					t.Fatalf("DriverDSN() error = %v, want a clear unsupported-username error", err)
				}
				if strings.Contains(err.Error(), "dsn-secret") {
					t.Fatalf("DriverDSN() error leaked password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DriverDSN(): %v", err)
			}
			parsed, err := mysql.ParseDSN(dsn)
			if err != nil {
				t.Fatalf("mysql.ParseDSN(): %v", err)
			}
			scope, err := ScopeFor(cfg)
			if err != nil {
				t.Fatalf("ScopeFor(): %v", err)
			}
			if parsed.User != scope.Principal {
				t.Fatalf("parsed user = %q, scope principal = %q", parsed.User, scope.Principal)
			}
		})
	}
}
