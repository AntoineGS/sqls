# InterBase Connection Configuration and Database Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the InterBase driver's real connection surface — structured host/port, `role`, `connectTimeout`, TLS and all five charsets — through sqls connection settings, and make a single-attachment connection report its own identity so `showDatabases`/`switchDatabase` stop being inert.

**Architecture:** A nested `interbase:` block on `DBConfig` carries the InterBase-only keys, validated by small untagged helpers that `DBConfig.Validate` and the connect path both call, so validation and mapping share one body. Those helpers produce `interBaseConnConfig`, a driver-neutral projection of the fields `interbase.Config` exposes; the tagged `interbase_native.go` copies it field-for-field into `interbase.Config`, which lets the whole mapping be unit-tested with plain `go test ./...` even though the driver package needs cgo. The repository gains the attachment string as its identity, and an optional `DatabaseSwitchRepository` interface lets `switchDatabase` refuse a name the connection cannot serve without teaching the handler anything about InterBase.

**Tech Stack:** Go 1.25, `database/sql`, `interbase-go` (cgo, behind the `interbase` build tag), `gopkg.in/yaml.v2`, table-driven tests in the package under test.

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-dialect-and-catalog-design.md` — this plan is **plan 3 of 3** in that spec's "Plan decomposition": §4.6 and §4.7, `internal/database/interbase_config_test.go`, README items 2/3/5, and the `schema.json` change.

## Global Constraints

Copied from the spec; every task's requirements implicitly include this section.

- "`DBRepository` (`internal/database/database.go:24`) does not change." No method is added to, removed from, or re-signed on that interface. New capability is an *optional* interface that callers type-assert.
- "InterBase attach, `Diagnostics` and `Plan` require cgo and the `interbase` build tag (`//go:build interbase && cgo && linux && amd64`); `schema` and DDL generation do not." Config types, validation and the driver-config mapping are **untagged** and must compile and be tested by plain `go test ./...`. The untagged package must not import `interbase-go` (its root package fails to build without cgo — verified: `CGO_ENABLED=0 go build interbase-go` reports `undefined: nativeConnection` and nine sibling errors).
- "Charset allowlist widened to `UTF8`, `WIN1250`, `WIN1252`, `ISO8859_1`, `ASCII`, empty meaning `UTF8` — the driver's exact set (`interbase.go:383-392`). sqls keeps its own copy because the driver's normalizer is unexported and validation must work on untagged builds; a test documents that the two lists must stay in sync."
- "`dataSourceName` still works as a raw attachment string, and the note that it cannot be combined with TLS."
- "**Security wording is a hard requirement.** Neither the README nor any log line may imply that enabling TLS authenticates the server."
- "The driver's TLS caveat is real and must not be softened": `README.md:119-125` of `interbase-go` — "with vendor client `LI-V15.1.0.42` a trusted CA does **not** establish server identity; an intentionally wrong DNS hostname was accepted, including through vendor `isql`."
- "Nothing is required. Every existing `config.yml` InterBase connection keeps working: `dataSourceName`, `host`/`port`/`path`/`dbName`, `user`, `passwd` and `params.charset` are unchanged."
- Explicitly deferred by the spec and **out of scope for this plan**: "`Config.EncryptedPassword` / `SystemEncryptionPassword`" and "`Config.TransactionOptions` (no-wait, record version, table reservation) and per-statement isolation". Do not add settings for them; if one seems necessary, report it instead of adding it.
- "Services-backed administration (backup, restore, sweep, user management) is out of scope for the entire project."
- InterBase-over-SSH stays rejected: `internal/database/config.go:146-148` must keep returning `"InterBase connections via SSH are not supported"`.
- **Plan dependency:** "Plan 1, which owns the `interbase.Config` construction site that plan 3 extends." Plan 1 owns `DBConfig.Dialect`, dialect resolution inside `interBaseOpen`, `DBConnection.Variant`/`DatabaseName`/`Warnings`, and `ConnFactory`/`RegisterConnFactory`/`CreateRepositoryFromConnection`. This plan **consumes** those; it never redefines them. Every edit to `interBaseOpen` below is written as an addition to the post-plan-1 shape and says so at the edit site.
- Commands: `go test ./...` for the untagged suite; `CGO_ENABLED=1 go test -tags interbase ./...` for the native suite (needs the InterBase client under `/opt/interbase`); `make test` is `build` then `go test -v ./...` (`Makefile:37`).
- Other agents are active in this repository. Commit **only** the files each task's commit step names. Never `git add -A` or `git add .`. If the index is locked, wait and retry once.

---

## File Structure

**Modified**

- `internal/database/config.go` — gains `InterBaseConfig` and `InterBaseTLSConfig`, the `DBConfig.InterBase` field, one cross-driver guard, and three validation calls inside the existing InterBase case. Validation stays a thin caller of helpers; no InterBase knowledge is inlined here, matching how `interBaseAttachment`/`interBaseCharset` are already called at `config.go:149-154`.
- `internal/database/interbase_common.go` — the untagged home for everything InterBase-specific and cgo-free: the widened charset allowlist, `interBaseRole`/`interBaseConnectTimeout`/`interBaseTLS`, the `interBaseConnConfig` projection and `interBaseConnectionConfig`, the repository's `DatabaseName` field, and the three methods that use it.
- `internal/database/interbase_native.go` — tagged; the only file that mentions `interbase.Config`. Gains `interBaseDriverConfig`, and its config literal inside `interBaseOpen` is replaced by a call to it.
- `internal/handler/execute_command.go` — `switchDatabase` consults the optional interface through one new package-level helper.
- `schema.json` — the `interbase` object.
- `README.md` — items 2, 3 and 5 of the spec's README list.

**Created**

- `internal/database/switch.go` — one optional interface, `DatabaseSwitchRepository`. It lives in its own file because it is driver-neutral and belongs to neither `interbase_common.go` (InterBase-specific) nor `capability.go` (created by plan 2, whose scope does not include it).
- `internal/database/interbase_config_test.go` — the whole offline suite for this plan.

**Test files extended**

- `internal/database/interbase_live_test.go` — two tagged, non-live tests that pin sqls's copies against the real driver. They dial nothing: `interbase.NewConnector` validates and returns (`interbase.go:114-139`).
- `internal/handler/interbase_test.go` — one test for the switch guard's wiring.

**Deliberately not touched**

- `internal/database/interbase_test.go`. Its `CurrentDatabase`/`Databases` assertions (`:18-23`) build the repository with `NewInterBaseDBRepository(db)`, which leaves `DatabaseName` empty, so they keep passing unchanged and become the regression guard for the "no identity" branch.

**Cross-plan note on `TestInterBaseCurrentDatabaseAndDatabases`:** the spec lists this test under plan 2's `interbase_catalog_test.go`. This plan owns it and places it in `interbase_config_test.go` (Task 4). Two files in one package cannot both declare it. If plan 2 has already added it, keep plan 2's copy, verify it asserts everything Task 4's version asserts, and add only the assertions it is missing.

---

### Task 1: Widen the charset allowlist to the driver's five

**Files:**
- Modify: `internal/database/interbase_common.go:65-91` (`interBaseCharset`)
- Test: `internal/database/interbase_config_test.go` (created here), `internal/database/interbase_live_test.go` (tagged addition)

**Interfaces:**
- Consumes: `interBaseCharset(cfg *DBConfig) (string, error)` (`interbase_common.go:65`) — keeps its exact signature and its conflicting-duplicate behavior.
- Produces: `var interBaseCharsets = []string{"UTF8", "WIN1250", "WIN1252", "ISO8859_1", "ASCII"}` — the allowlist later tasks and the tagged drift test read.

- [ ] **Step 1: Write the failing test**

Create `internal/database/interbase_config_test.go` with this content:

```go
package database

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run '^TestInterBaseConfigValidatesCharset$|^TestInterBaseCharsetMatchesDriverAllowlist$' -v`

Expected: FAIL — build error `undefined: interBaseCharsets`, and once that is stubbed the `WIN1252`/`ISO8859_1`/`ASCII` subtests fail with `interbase: unsupported charset`.

- [ ] **Step 3: Widen the allowlist**

In `internal/database/interbase_common.go`, add `"slices"` to the import block and insert this above `interBaseCharset` (before the current line 65):

```go
// interBaseCharsets mirrors the driver's normalizeCharset allowlist
// (interbase-go interbase.go:383-392). The driver's normalizer is unexported and
// its package only builds with cgo, so sqls keeps this copy in order to validate
// a connection on an untagged build.
var interBaseCharsets = []string{"UTF8", "WIN1250", "WIN1252", "ISO8859_1", "ASCII"}
```

Replace the trailing switch of `interBaseCharset` (`interbase_common.go:83-90`):

```go
	switch strings.ToUpper(strings.TrimSpace(charset)) {
	case "":
		return "UTF8", nil
	case "UTF8", "WIN1250":
		return strings.ToUpper(strings.TrimSpace(charset)), nil
	default:
		return "", fmt.Errorf("interbase: unsupported charset %q", charset)
	}
```

with:

```go
	normalized := strings.ToUpper(strings.TrimSpace(charset))
	if normalized == "" {
		return "UTF8", nil
	}
	if slices.Contains(interBaseCharsets, normalized) {
		return normalized, nil
	}
	return "", fmt.Errorf("interbase: unsupported charset %q", charset)
```

Nothing above line 83 changes: the duplicate-parameter loop and its conflicting-value error stay exactly as they are.

- [ ] **Step 4: Run the tests to verify they pass**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBaseConfigValidatesCharset$|^TestInterBaseCharsetMatchesDriverAllowlist$' -v
go test ./internal/database/ -count=1 -run '^TestInterBaseConfig' -v
```

Expected: PASS. The second command includes `TestInterBaseConfigBuildsLocalAndRemoteAttachmentsWithoutSecrets` and `TestInterBaseConfigRejectsUnsupportedConnectionModes` (`interbase_test.go:124`, `:201`), whose `win1250` and `latin1` expectations must be unaffected.

- [ ] **Step 5: Add the tagged drift guard**

Append to `internal/database/interbase_live_test.go`, and add `interbase "interbase-go"` to its import block:

```go
func TestInterBaseCharsetAllowlistMatchesDriverNormalizer(t *testing.T) {
	// NewConnector validates and returns without dialing (interbase.go:114-139),
	// so this test needs no server. It fails the moment the driver's allowlist and
	// interBaseCharsets disagree in either direction.
	for _, charset := range interBaseCharsets {
		if _, err := interbase.NewConnector(interbase.Config{
			Database: "/tmp/sqls-allowlist.ib",
			User:     "sqls",
			Charset:  charset,
		}); err != nil {
			t.Errorf("driver rejected charset %q that sqls accepts: %v", charset, err)
		}
	}
	if _, err := interbase.NewConnector(interbase.Config{
		Database: "/tmp/sqls-allowlist.ib",
		User:     "sqls",
		Charset:  "LATIN1",
	}); err == nil {
		t.Error("driver accepted charset LATIN1 that sqls rejects; the allowlists have drifted")
	}
}
```

- [ ] **Step 6: Build and run the tagged suite**

Run:

```sh
CGO_ENABLED=1 go build -tags interbase ./...
CGO_ENABLED=1 go test -tags interbase ./internal/database/ -count=1 -run '^TestInterBaseCharsetAllowlistMatchesDriverNormalizer$' -v
```

Expected: PASS. Run these only where the InterBase client is installed under `/opt/interbase`; without it the tagged build cannot link, which is a toolchain gap, not a test failure. Record in the task report whether the tagged command ran.

- [ ] **Step 7: Run the full untagged suite**

Run: `go test ./... 2>&1 | tail -30`

Expected: no failures.

- [ ] **Step 8: Commit**

```bash
git add internal/database/interbase_common.go internal/database/interbase_config_test.go internal/database/interbase_live_test.go
git commit -m "Accept the five character sets the InterBase driver supports"
```

---

### Task 2: The `interbase` connection block, its validation, and the role/timeout/TLS helpers

**Files:**
- Modify: `internal/database/config.go:21-34` (the `DBConfig` struct), `internal/database/config.go:142-154` (the InterBase validation case), and one guard near `config.go:40-42`
- Modify: `internal/database/interbase_common.go` (new helpers below `interBaseCharset`)
- Test: `internal/database/interbase_config_test.go`

**Interfaces:**
- Consumes: `interBaseCharsets` and `interBaseCharset` (Task 1); `interBaseAttachment(cfg *DBConfig) (string, error)` (`interbase_common.go:24`), unchanged.
- Produces:
  - `type InterBaseConfig struct { Role string; ConnectTimeout string; TLS *InterBaseTLSConfig }`
  - `type InterBaseTLSConfig struct { Enabled bool; ServerPublicFile, ServerPublicPath, ClientCertFile, ClientPassPhrase, ClientPassPhraseFile string }`
  - `DBConfig.InterBase *InterBaseConfig`
  - `func interBaseRole(cfg *DBConfig) (string, error)`
  - `func interBaseConnectTimeout(cfg *DBConfig) (time.Duration, error)`
  - `func interBaseTLS(cfg *DBConfig) (interBaseTLSSettings, error)`
  - `type interBaseTLSSettings struct { Enabled bool; ServerPublicFile, ServerPublicPath, ClientCertFile, ClientPassPhrase, ClientPassPhraseFile string }` with `func (t interBaseTLSSettings) hasOptions() bool`

Task 3 consumes all six; the tagged file consumes `interBaseTLSSettings`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/database/interbase_config_test.go`, and extend its import block to `"reflect"`, `"strings"`, `"testing"`, `"time"`, `"github.com/sqls-server/sqls/dialect"`, `"gopkg.in/yaml.v2"`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run '^TestInterBase(ConfigValidatesConnectionOptions|ConfigStillRejectsSSHTunnelling|ConnectionOptionsUnmarshalFromYAML)$' -v`

Expected: FAIL — build errors `undefined: InterBaseConfig`, `undefined: InterBaseTLSConfig`, `unknown field InterBase in struct literal`, `undefined: interBaseConnectTimeout`.

- [ ] **Step 3: Add the config types and the `InterBase` field**

In `internal/database/config.go`, add the field to `DBConfig` (after `SSHCfg`, `config.go:33`). Plan 1 adds a `Dialect int` field to this same struct; both are additive and order does not matter — if `Dialect` is already there, put `InterBase` directly after it:

```go
	InterBase      *InterBaseConfig       `json:"interbase" yaml:"interbase"`
```

Add both types below `SSHConfig`'s methods, at the end of `config.go`:

```go
// InterBaseConfig holds settings that only the InterBase driver understands.
// The nested block keeps InterBase-only keys out of the shared DBConfig surface,
// matching the existing sshConfig precedent.
type InterBaseConfig struct {
	Role string `json:"role" yaml:"role"`
	// ConnectTimeout bounds the native attachment handshake. It is a Go duration
	// string, for example "10s"; empty leaves the InterBase client default. It is
	// a string because YAML has no duration type and a bare integer is ambiguous.
	ConnectTimeout string              `json:"connectTimeout" yaml:"connectTimeout"`
	TLS            *InterBaseTLSConfig `json:"tls" yaml:"tls"`
}

// InterBaseTLSConfig holds the InterBase native client TLS attachment options.
// Enabling TLS encrypts the connection; it is not proof of server identity. See
// the TLS note in README.md before relying on it.
type InterBaseTLSConfig struct {
	Enabled              bool   `json:"enabled" yaml:"enabled"`
	ServerPublicFile     string `json:"serverPublicFile" yaml:"serverPublicFile"`
	ServerPublicPath     string `json:"serverPublicPath" yaml:"serverPublicPath"`
	ClientCertFile       string `json:"clientCertFile" yaml:"clientCertFile"`
	ClientPassPhrase     string `json:"clientPassPhrase" yaml:"clientPassPhrase"`
	ClientPassPhraseFile string `json:"clientPassPhraseFile" yaml:"clientPassPhraseFile"`
}
```

- [ ] **Step 4: Add the three helpers**

In `internal/database/interbase_common.go`, extend the import block with `"time"` and append below `interBaseCharset`:

```go
// interBaseMaxRoleBytes mirrors the driver's credential length limit
// (interbase-go interbase.go:165-169, math.MaxUint8).
const interBaseMaxRoleBytes = 255

// interBaseTLSSettings is the validated projection of InterBaseTLSConfig. It
// exists separately so the untagged package never names interbase.TLSConfig.
type interBaseTLSSettings struct {
	Enabled              bool
	ServerPublicFile     string
	ServerPublicPath     string
	ClientCertFile       string
	ClientPassPhrase     string
	ClientPassPhraseFile string
}

// hasOptions mirrors interbase.TLSConfig.hasOptions (interbase.go:32-36): an
// enabled flag alone already counts as a TLS option.
func (t interBaseTLSSettings) hasOptions() bool {
	return t.Enabled || t.ServerPublicFile != "" || t.ServerPublicPath != "" ||
		t.ClientCertFile != "" || t.ClientPassPhrase != "" || t.ClientPassPhraseFile != ""
}

func interBaseRole(cfg *DBConfig) (string, error) {
	if cfg == nil {
		return "", errors.New("interbase: connection config is nil")
	}
	if cfg.InterBase == nil {
		return "", nil
	}
	role := cfg.InterBase.Role
	if strings.IndexByte(role, 0) >= 0 {
		return "", errors.New("invalid: connections[].interbase.role cannot contain NUL bytes")
	}
	if len(role) > interBaseMaxRoleBytes {
		return "", fmt.Errorf("invalid: connections[].interbase.role cannot exceed %d bytes", interBaseMaxRoleBytes)
	}
	return role, nil
}

func interBaseConnectTimeout(cfg *DBConfig) (time.Duration, error) {
	if cfg == nil {
		return 0, errors.New("interbase: connection config is nil")
	}
	if cfg.InterBase == nil {
		return 0, nil
	}
	value := strings.TrimSpace(cfg.InterBase.ConnectTimeout)
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid: connections[].interbase.connectTimeout %q is not a Go duration such as \"10s\"", value)
	}
	if timeout < 0 {
		return 0, errors.New("invalid: connections[].interbase.connectTimeout cannot be negative")
	}
	return timeout, nil
}

// interBaseTLS validates the TLS block and returns it in driver terms. TLS needs
// a structured host because the driver composes the attachment itself and
// rejects TLS options when Config.Host is empty (interbase.go:190-195); a
// hand-built dataSourceName therefore cannot carry TLS. Failing here rather than
// at attach time gives the user the offending configuration key.
func interBaseTLS(cfg *DBConfig) (interBaseTLSSettings, error) {
	if cfg == nil {
		return interBaseTLSSettings{}, errors.New("interbase: connection config is nil")
	}
	if cfg.InterBase == nil || cfg.InterBase.TLS == nil {
		return interBaseTLSSettings{}, nil
	}
	tls := interBaseTLSSettings{
		Enabled:              cfg.InterBase.TLS.Enabled,
		ServerPublicFile:     cfg.InterBase.TLS.ServerPublicFile,
		ServerPublicPath:     cfg.InterBase.TLS.ServerPublicPath,
		ClientCertFile:       cfg.InterBase.TLS.ClientCertFile,
		ClientPassPhrase:     cfg.InterBase.TLS.ClientPassPhrase,
		ClientPassPhraseFile: cfg.InterBase.TLS.ClientPassPhraseFile,
	}
	if !tls.hasOptions() {
		return interBaseTLSSettings{}, nil
	}
	if !tls.Enabled {
		return interBaseTLSSettings{}, errors.New("invalid: connections[].interbase.tls options require connections[].interbase.tls.enabled")
	}
	if cfg.DataSourceName != "" || cfg.Host == "" {
		return interBaseTLSSettings{}, errors.New("invalid: connections[].interbase.tls requires connections[].host")
	}
	return tls, nil
}
```

No error message here interpolates a TLS option value or a password, which is what the no-leak assertions pin.

The order of the last two checks is deliberate. The driver checks the host first and so reports "TLS options require a host" for a hostless config whose real mistake is a missing `enabled` flag (`interbase.go:190-195` runs before the `!tls.Enabled` guard at `:205-207`). Checking `enabled` first names the key the user actually has to change.

- [ ] **Step 5: Wire the validation**

In `internal/database/config.go`, add the cross-driver guard immediately after the empty-driver check (`config.go:40-42`), before the `switch c.Driver`. Plan 1 adds a sibling guard for `c.Dialect` at this same place; if it is already present, put this one directly beneath it:

```go
	if c.InterBase != nil && c.Driver != dialect.DatabaseDriverInterBase {
		return errors.New("invalid: connections[].interbase is only supported by the interbase driver")
	}
```

Then extend the InterBase case (`config.go:142-154`) so it reads:

```go
	case dialect.DatabaseDriverInterBase:
		if c.User == "" {
			return errors.New("required: connections[].user")
		}
		if c.SSHCfg != nil {
			return errors.New("InterBase connections via SSH are not supported")
		}
		if _, err := interBaseAttachment(c); err != nil {
			return err
		}
		if _, err := interBaseCharset(c); err != nil {
			return err
		}
		if _, err := interBaseRole(c); err != nil {
			return err
		}
		if _, err := interBaseConnectTimeout(c); err != nil {
			return err
		}
		if _, err := interBaseTLS(c); err != nil {
			return err
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBase' -v
go test ./internal/config/ -count=1
```

Expected: PASS for both. `internal/config` is included because `DBConfig` grew a field that its testdata round-trips.

- [ ] **Step 7: Run the full untagged suite and vet**

Run:

```sh
go test ./... 2>&1 | tail -30
go vet ./...
```

Expected: no failures, no vet output.

- [ ] **Step 8: Commit**

```bash
git add internal/database/config.go internal/database/interbase_common.go internal/database/interbase_config_test.go
git commit -m "Add the interbase connection block with role, connectTimeout and TLS validation"
```

---

### Task 3: Map the connection settings onto the driver's structured config

This is the task that makes TLS reachable at all: the driver builds the attachment from `Config.Host` plus `Config.Database` (`interbase.go:185-239`), so sqls must stop hand-building `host/port:path` and hand it the parts instead.

**Files:**
- Modify: `internal/database/interbase_common.go` (append `interBaseConnConfig` and `interBaseConnectionConfig`)
- Modify: `internal/database/interbase_native.go` (post-plan-1 shape; see Step 4)
- Test: `internal/database/interbase_config_test.go`, `internal/database/interbase_live_test.go`

**Interfaces:**
- Consumes: `interBaseAttachment`, `interBaseCharset`, `interBaseRole`, `interBaseConnectTimeout`, `interBaseTLS`, `interBaseTLSSettings`, `interBaseDefaultPort` (`interbase_common.go:13`).
- Produces:
  - `type interBaseConnConfig struct { Database, Host, User, Password, Role, Charset string; ConnectTimeout time.Duration; TLS interBaseTLSSettings }`
  - `func interBaseConnectionConfig(cfg *DBConfig) (interBaseConnConfig, error)`
  - `func interBaseDriverConfig(cfg interBaseConnConfig, sqlDialect int) interbase.Config` — tagged only.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_config_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run '^TestInterBase(DriverConfigMapping|ConnectionConfigRejectsInvalidSettings)$' -v`

Expected: FAIL — build errors `undefined: interBaseConnConfig`, `undefined: interBaseConnectionConfig`.

- [ ] **Step 3: Add the mapping helper**

Append to `internal/database/interbase_common.go`:

```go
// interBaseConnConfig is the driver-neutral projection of a DBConfig onto the
// fields interbase.Config exposes. It exists because the driver's package needs
// cgo and the interbase build tag, while this mapping and its tests must build
// with plain `go test ./...`; interbase_native.go copies it field-for-field.
type interBaseConnConfig struct {
	Database       string
	Host           string
	User           string
	Password       string
	Role           string
	Charset        string
	ConnectTimeout time.Duration
	TLS            interBaseTLSSettings
}

// interBaseConnectionConfig maps the connection settings onto the driver's
// structured configuration. Host and Database are handed over separately so the
// driver composes the attachment itself, which is the only way TLS options can
// be carried (interbase.go:185-239). A dataSourceName stays a raw attachment
// string with no host, exactly as before.
//
// This mirrors interBaseAttachment's composition in structured form rather than
// sharing it, because interBaseAttachment must keep producing the exact display
// string its existing tests pin. TestInterBaseDriverConfigMapping recomposes
// Host + ":" + Database and asserts it equals interBaseAttachment's output for
// every case, so add a case there when adding a branch to either function.
func interBaseConnectionConfig(cfg *DBConfig) (interBaseConnConfig, error) {
	// Shares the proto, port, host and path validation with DBConfig.Validate.
	if _, err := interBaseAttachment(cfg); err != nil {
		return interBaseConnConfig{}, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return interBaseConnConfig{}, err
	}
	role, err := interBaseRole(cfg)
	if err != nil {
		return interBaseConnConfig{}, err
	}
	connectTimeout, err := interBaseConnectTimeout(cfg)
	if err != nil {
		return interBaseConnConfig{}, err
	}
	tls, err := interBaseTLS(cfg)
	if err != nil {
		return interBaseConnConfig{}, err
	}

	conn := interBaseConnConfig{
		User:           cfg.User,
		Password:       cfg.Passwd,
		Role:           role,
		Charset:        charset,
		ConnectTimeout: connectTimeout,
		TLS:            tls,
	}
	if cfg.DataSourceName != "" {
		conn.Database = cfg.DataSourceName
		return conn, nil
	}

	conn.Database = cfg.Path
	if conn.Database == "" {
		conn.Database = cfg.DBName
	}
	if cfg.Host != "" {
		port := cfg.Port
		if port == 0 {
			port = interBaseDefaultPort
		}
		conn.Host = fmt.Sprintf("%s/%d", cfg.Host, port)
	}
	return conn, nil
}
```

`interBaseAttachment` keeps its current body and its current output. It is now the *display* attachment used for `DBConnection.DatabaseName`, `showConnections` and error messages, and it deliberately omits TLS parameters: the driver's composed attachment can contain `clientPassPhrase`, and the display string is echoed back to the user by `showDatabases`.

Add the converse note above it, so the pairing is documented from both sides. The recompose assertion only catches drift for composition shapes already in the mapping table, so a *new* branch added to one function and not the other is the one failure it cannot see:

```go
// interBaseAttachment composes the display attachment string. Its output is
// pinned by existing tests and is what the user sees in showDatabases and
// showConnections, so it must not gain TLS parameters.
//
// interBaseConnectionConfig mirrors this composition in structured form for the
// driver. TestInterBaseDriverConfigMapping pins the two together by recomposing
// Host + ":" + Database; add a case there when adding a branch here.
func interBaseAttachment(cfg *DBConfig) (string, error) {
```

- [ ] **Step 4: Use the mapping in the tagged connect path**

`internal/database/interbase_native.go` today builds the config inline (`:34-39`):

```go
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := interbase.NewConnector(interbase.Config{
		Database: attachment,
		User:     cfg.User,
		Password: cfg.Passwd,
		Charset:  charset,
	})
```

**Plan 1 owns this function's dialect flow** and moves that literal inside an `attach(requested int)` helper that it calls once, or twice for an auto-detected Dialect 1 database. Apply this task's change on top of whatever shape plan 1 left, without touching the dialect switch, the `interbase.Diagnostics` call, the warnings or the returned `DBConnection` fields:

1. Delete the `charset, err := interBaseCharset(cfg)` block. The charset now arrives inside `connCfg`.
2. Immediately after the `attachment, err := interBaseAttachment(cfg)` block, add:

```go
	connCfg, err := interBaseConnectionConfig(cfg)
	if err != nil {
		return nil, err
	}
```

3. Replace the `interbase.Config{...}` literal with `interBaseDriverConfig(connCfg, sqlDialect)`, where `sqlDialect` is plan 1's dialect expression at that call site, copied verbatim. If plan 1 wrapped the attach in a closure, the call becomes `interbase.NewConnector(interBaseDriverConfig(connCfg, requested))` inside it.
4. Append the converter to the same file:

```go
// interBaseDriverConfig copies the untagged projection into the driver's
// configuration. Host and Database stay separate so the driver composes the
// attachment, including any TLS options.
func interBaseDriverConfig(cfg interBaseConnConfig, sqlDialect int) interbase.Config {
	return interbase.Config{
		Database:       cfg.Database,
		Host:           cfg.Host,
		User:           cfg.User,
		Password:       cfg.Password,
		Role:           cfg.Role,
		Charset:        cfg.Charset,
		Dialect:        sqlDialect,
		ConnectTimeout: cfg.ConnectTimeout,
		TLS: interbase.TLSConfig{
			Enabled:              cfg.TLS.Enabled,
			ServerPublicFile:     cfg.TLS.ServerPublicFile,
			ServerPublicPath:     cfg.TLS.ServerPublicPath,
			ClientCertFile:       cfg.TLS.ClientCertFile,
			ClientPassPhrase:     cfg.TLS.ClientPassPhrase,
			ClientPassPhraseFile: cfg.TLS.ClientPassPhraseFile,
		},
	}
}
```

`interbase.Config` also has `EncryptedPassword`, `SystemEncryptionPassword` and `TransactionOptions`. The spec defers all three; leaving them at their zero values is intentional, not an omission.

- [ ] **Step 5: Add the tagged acceptance test**

Append to `internal/database/interbase_live_test.go`:

```go
func TestInterBaseDriverConfigIsAcceptedByConnector(t *testing.T) {
	// NewConnector validates the whole configuration, including the composed
	// attachment string, without dialing (interbase.go:114-139). This is the
	// guard that sqls's mapping produces something the driver accepts.
	cfg := &DBConfig{
		Driver: dialect.DatabaseDriverInterBase,
		Host:   "db.example.test",
		Path:   "/srv/interbase/example.ib",
		User:   "alice",
		Passwd: "do-not-log",
		InterBase: &InterBaseConfig{
			Role:           "SQLS_READONLY",
			ConnectTimeout: "10s",
			TLS: &InterBaseTLSConfig{
				Enabled:              true,
				ServerPublicFile:     "/etc/interbase/server.pem",
				ClientPassPhraseFile: "/etc/interbase/client.pass",
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DBConfig.Validate() error = %v", err)
	}
	connCfg, err := interBaseConnectionConfig(cfg)
	if err != nil {
		t.Fatalf("interBaseConnectionConfig() error = %v", err)
	}
	if _, err := interbase.NewConnector(interBaseDriverConfig(connCfg, 3)); err != nil {
		t.Fatalf("driver rejected the mapped TLS configuration: %v", err)
	}

	// The same settings without a host are what sqls refuses in validation; the
	// driver refuses them too, so the two agree on the rule.
	connCfg.Host = ""
	if _, err := interbase.NewConnector(interBaseDriverConfig(connCfg, 3)); err == nil {
		t.Fatal("driver accepted TLS options with no host")
	}
}
```

- [ ] **Step 6: Run the tests**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBase' -v
go test ./... 2>&1 | tail -30
CGO_ENABLED=1 go build -tags interbase ./...
CGO_ENABLED=1 go test -tags interbase ./internal/database/ -count=1 -run '^TestInterBaseDriverConfigIsAcceptedByConnector$' -v
```

Expected: PASS for the untagged commands, and a clean tagged build.

**`CGO_ENABLED=1 go build -tags interbase ./...` is mandatory for this task, not optional.** Step 4 removes `Database: attachment` from the config literal in `interbase_native.go`, and `attachment` only stays live because plan 1 assigns it to `DBConnection.DatabaseName`. If this task is executed before plan 1 has landed, `attachment` becomes an unused variable and that file will not compile — and the untagged suite cannot see it, because `interbase_native.go` is behind the build tag. The tagged build is the only check that catches it. `/opt/interbase` is present on the development machine, so there is no excuse to skip it; if the build fails with `declared and not used: attachment`, plan 1 has not landed and this task must wait rather than be worked around by deleting the variable.

The tagged *test* on the last line additionally needs a reachable server for the rest of the live suite's environment gating; run it where the client is installed and report whether it ran.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_common.go internal/database/interbase_native.go internal/database/interbase_config_test.go internal/database/interbase_live_test.go
git commit -m "Hand InterBase structured host, role, timeout and TLS settings"
```

---

### Task 4: Database identity for a single-attachment connection

**Files:**
- Modify: `internal/database/interbase_common.go:93-115` (the repository struct and the two identity methods)
- Test: `internal/database/interbase_config_test.go`

**Interfaces:**
- Consumes: `DBConnection.DatabaseName` and `NewInterBaseDBRepositoryFromConnection(conn *DBConnection) DBRepository`, both established by plan 1 (spec §4.1), where `DBConnection.DatabaseName` holds the display attachment from `interBaseAttachment`.
- Produces: `InterBaseDBRepository.DatabaseName string`; `CurrentDatabase` returns it; `Databases` returns `[]string{DatabaseName}`, or `[]string{}` when it is empty.

- [ ] **Step 1: Check what the earlier plans already left in place**

Run: `grep -n "DatabaseName\|SQLDialect" internal/database/interbase_common.go internal/database/driver.go`

Spec §4.3 declares `InterBaseDBRepository` with both `SQLDialect` and `DatabaseName`, and plan 1's connection-aware factory populates them. Two outcomes:

- The field `DatabaseName string` already exists on `InterBaseDBRepository` — do not declare it again. Skip the struct edit in Step 3 and check in Step 4 that the connection-aware factory sets it from `conn.DatabaseName`.
- It does not exist — apply Step 3 exactly as written.

Either way, Steps 2 and 5 of this task run unchanged.

- [ ] **Step 2: Write the failing test**

**Known cross-plan collision, restated here because the File Structure note is far above:** the spec lists `TestInterBaseCurrentDatabaseAndDatabases` under plan 2's `interbase_catalog_test.go`, and two files in one Go package cannot both declare it. If plan 2 has already landed its copy, keep that one, add only the assertions below that it is missing, and skip the rest of this step. If it has not, this file owns the test.

Append to `internal/database/interbase_config_test.go`, adding `"context"` to its imports:

```go
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run '^TestInterBaseCurrentDatabaseAndDatabases$' -v`

Expected: FAIL — either `unknown field DatabaseName in struct literal` (field absent) or, if plan 1 or 2 already added it, `CurrentDatabase() = ("", <nil>), want ("db.example.test/3050:/srv/interbase/centrale.ib", nil)`.

- [ ] **Step 4: Report the attachment as the database identity**

In `internal/database/interbase_common.go`, give the repository the field — unless Step 1 found it already there:

```go
type InterBaseDBRepository struct {
	Conn *sql.DB
	// DatabaseName is the display attachment string for this connection; empty
	// when the repository was built without connection context.
	DatabaseName string
}
```

Replace the two identity methods (`interbase_common.go:107-115`) with:

```go
// InterBase serves exactly one database per attachment, so the attachment string
// is the connection's identity. It is used verbatim rather than shortened to a
// basename, because it is what the user configured and it disambiguates remote
// attachments: two hosts can serve /srv/data/x.ib.
func (db *InterBaseDBRepository) CurrentDatabase(context.Context) (string, error) {
	return db.DatabaseName, nil
}

func (db *InterBaseDBRepository) Databases(context.Context) ([]string, error) {
	if db.DatabaseName == "" {
		return []string{}, nil
	}
	return []string{db.DatabaseName}, nil
}
```

Then confirm plan 1's `NewInterBaseDBRepositoryFromConnection` sets `DatabaseName: conn.DatabaseName`. If it does not, add that one field to its returned literal and leave everything else in that constructor alone. `NewInterBaseDBRepository(conn *sql.DB)` keeps its signature and leaves the field empty.

- [ ] **Step 5: Run the tests to verify they pass**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBase' -v
go test ./... 2>&1 | tail -30
```

Expected: PASS, including the untouched `TestInterBaseCatalogRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys`, whose empty-identity assertions (`interbase_test.go:18-23`) are the regression guard for the anonymous branch.

- [ ] **Step 6: Commit**

```bash
git add internal/database/interbase_common.go internal/database/interbase_config_test.go
git commit -m "Report the InterBase attachment as the connection's database"
```

---

### Task 5: `switchDatabase` refuses a database this connection cannot serve

Without this, `showDatabases` now prints the attachment and `switchDatabase` still silently accepts any name and reconnects. Note that `newDBConnection` assigns `connCfg.DBName = s.curDBName` (`internal/handler/handler.go:373-375`), so on a connection configured with only `dbName` today's `switchDatabase` really does re-attach somewhere else; refusing it is the intended behavior change.

**Files:**
- Create: `internal/database/switch.go`
- Modify: `internal/database/interbase_common.go` (one method)
- Modify: `internal/handler/execute_command.go:381-399` (`switchDatabase`)
- Test: `internal/database/interbase_config_test.go`, `internal/handler/interbase_test.go`

**Interfaces:**
- Consumes: `InterBaseDBRepository.DatabaseName` (Task 4); `(*Server).newDBRepository(ctx) (database.DBRepository, error)` (`handler.go:386`), which returns `ErrNoConnection` when `s.curDBCfg` or `s.dbConn` is nil.
- Produces:
  - `type DatabaseSwitchRepository interface { ValidateDatabaseSwitch(ctx context.Context, name string) error }`
  - `func (db *InterBaseDBRepository) ValidateDatabaseSwitch(_ context.Context, name string) error`
  - `func validateDatabaseSwitch(ctx context.Context, repo database.DBRepository, name string) error` in package `handler`

- [ ] **Step 1: Write the failing tests**

Append to `internal/database/interbase_config_test.go`:

```go
func TestInterBaseValidatesDatabaseSwitch(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	ctx := context.Background()
	repository := &InterBaseDBRepository{DatabaseName: attachment}

	if err := repository.ValidateDatabaseSwitch(ctx, attachment); err != nil {
		t.Fatalf("ValidateDatabaseSwitch(current) error = %v, want nil", err)
	}
	if err := repository.ValidateDatabaseSwitch(ctx, "  "+attachment+"  "); err != nil {
		t.Fatalf("ValidateDatabaseSwitch(padded current) error = %v, want nil", err)
	}

	err := repository.ValidateDatabaseSwitch(ctx, "/srv/interbase/other.ib")
	if err == nil {
		t.Fatal("ValidateDatabaseSwitch(other) returned nil error")
	}
	for _, want := range []string{"single attachment", "another connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateDatabaseSwitch(other) error = %q, want mention %q", err, want)
		}
	}

	// Without a known identity the connection keeps today's permissive behavior
	// rather than blocking the user on a build path that cannot tell.
	if err := (&InterBaseDBRepository{}).ValidateDatabaseSwitch(ctx, "anything"); err != nil {
		t.Fatalf("ValidateDatabaseSwitch() on an anonymous repository error = %v, want nil", err)
	}
}

func TestOnlyInterBaseConstrainsDatabaseSwitching(t *testing.T) {
	var _ DatabaseSwitchRepository = (*InterBaseDBRepository)(nil)

	repositories := map[string]DBRepository{
		"mysql":      NewMySQLDBRepository(nil),
		"postgresql": NewPostgreSQLDBRepository(nil),
		"sqlite3":    NewSQLite3DBRepository(nil),
		"mssql":      NewMssqlDBRepository(nil),
		"h2":         NewH2DBRepository(nil),
		"vertica":    NewVerticaDBRepository(nil),
		"oracle":     NewOracleDBRepository(nil),
		"mock":       NewMockDBRepository(nil),
	}
	for name, repository := range repositories {
		if _, ok := repository.(DatabaseSwitchRepository); ok {
			t.Errorf("%s repository implements DatabaseSwitchRepository; the switch guard must stay InterBase-only", name)
		}
	}
}
```

Append to `internal/handler/interbase_test.go`:

```go
func TestSwitchDatabaseGuardRefusesAnotherAttachment(t *testing.T) {
	const attachment = "db.example.test/3050:/srv/interbase/centrale.ib"
	ctx := context.Background()
	repository := &database.InterBaseDBRepository{DatabaseName: attachment}

	if err := validateDatabaseSwitch(ctx, repository, attachment); err != nil {
		t.Fatalf("validateDatabaseSwitch(current) error = %v, want nil", err)
	}
	if err := validateDatabaseSwitch(ctx, repository, "/srv/interbase/other.ib"); err == nil {
		t.Fatal("validateDatabaseSwitch(other) returned nil error")
	}
	if err := validateDatabaseSwitch(ctx, &database.MockDBRepository{}, "world"); err != nil {
		t.Fatalf("a repository without the capability must accept any name: %v", err)
	}
}
```

Check `internal/handler/interbase_test.go`'s import block and add `"context"` and `"github.com/sqls-server/sqls/internal/database"` if they are not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBaseValidatesDatabaseSwitch$|^TestOnlyInterBaseConstrainsDatabaseSwitching$' -v
go test ./internal/handler/ -count=1 -run '^TestSwitchDatabaseGuardRefusesAnotherAttachment$' -v
```

Expected: FAIL — `undefined: DatabaseSwitchRepository`, `repository.ValidateDatabaseSwitch undefined`, `undefined: validateDatabaseSwitch`.

- [ ] **Step 3: Add the optional interface**

Create `internal/database/switch.go`:

```go
package database

import "context"

// DatabaseSwitchRepository is implemented by repositories whose connection is
// bound to a fixed set of databases. The switchDatabase command consults it
// before reconnecting; a repository that does not implement it accepts every
// name, which is the existing behavior for every driver that enumerates
// databases. DBRepository is unchanged: callers type-assert, and absence is the
// normal case.
type DatabaseSwitchRepository interface {
	// ValidateDatabaseSwitch returns nil when this connection can serve name and
	// an explanatory error when it cannot.
	ValidateDatabaseSwitch(ctx context.Context, name string) error
}
```

- [ ] **Step 4: Implement it for InterBase**

Append to `internal/database/interbase_common.go`, below `Databases`:

```go
var _ DatabaseSwitchRepository = (*InterBaseDBRepository)(nil)

// ValidateDatabaseSwitch accepts the attachment this connection already holds —
// switching to it is a harmless refresh — and refuses anything else, because an
// InterBase attachment cannot move to another database. Names are compared
// verbatim apart from surrounding blanks: an attachment string contains a file
// path, which is case sensitive on the servers sqls supports.
func (db *InterBaseDBRepository) ValidateDatabaseSwitch(_ context.Context, name string) error {
	if db.DatabaseName == "" || strings.TrimSpace(name) == strings.TrimSpace(db.DatabaseName) {
		return nil
	}
	return errors.New("interbase: this connection has a single attachment; configure another connection to open a different database")
}
```

- [ ] **Step 5: Consult it from the handler**

In `internal/handler/execute_command.go`, add the helper directly above `switchDatabase` (before `:381`):

```go
// validateDatabaseSwitch asks the repository whether it can serve dbName.
// Repositories that do not implement database.DatabaseSwitchRepository accept
// every name, which is the behavior every driver had before InterBase.
func validateDatabaseSwitch(ctx context.Context, repo database.DBRepository, dbName string) error {
	switcher, ok := repo.(database.DatabaseSwitchRepository)
	if !ok {
		return nil
	}
	return switcher.ValidateDatabaseSwitch(ctx, dbName)
}
```

Then insert the consultation into `switchDatabase`, between the argument parsing and `s.curDBName = dbName`:

```go
	// Only consult an open connection. With none open, switchDatabase is how a
	// user selects the database to connect to, so there is nothing to validate.
	if s.dbConn != nil && s.curDBCfg != nil {
		repo, err := s.newDBRepository(ctx)
		if err != nil {
			return nil, err
		}
		if err := validateDatabaseSwitch(ctx, repo, dbName); err != nil {
			return nil, err
		}
	}
```

The guard only bites when the repository knows its identity, which it does once plan 1's `newDBRepository` builds it through `CreateRepositoryFromConnection`.

- [ ] **Step 6: Run the tests to verify they pass**

Run:

```sh
go test ./internal/database/ -count=1 -run '^TestInterBaseValidatesDatabaseSwitch$|^TestOnlyInterBaseConstrainsDatabaseSwitching$' -v
go test ./internal/handler/ -count=1 -run '^TestSwitchDatabaseGuardRefusesAnotherAttachment$' -v
go test ./... 2>&1 | tail -30
go vet ./...
```

Expected: PASS everywhere, no vet output.

- [ ] **Step 7: Commit**

```bash
git add internal/database/switch.go internal/database/interbase_common.go internal/database/interbase_config_test.go internal/handler/execute_command.go internal/handler/interbase_test.go
git commit -m "Refuse switchDatabase to a database a single attachment cannot serve"
```

---

### Task 6: Documentation — `schema.json` and README items 2, 3 and 5

The TLS paragraph is the deliverable a reviewer should gate hardest: it must not imply that enabling TLS authenticates the server.

**Files:**
- Modify: `schema.json:8-97` (the connection item's `properties`)
- Modify: `README.md:290-348` (the InterBase section)

**Interfaces:**
- Consumes: the setting names produced by Tasks 1–5 — `interbase.role`, `interbase.connectTimeout`, `interbase.tls.{enabled,serverPublicFile,serverPublicPath,clientCertFile,clientPassPhrase,clientPassPhraseFile}`, the five charsets, and the single-attachment `switchDatabase` rule.
- Produces: no code.

- [ ] **Step 1: Add `interbase` to `schema.json`**

The connection item sets `"additionalProperties": false` (`schema.json:9`), so an undeclared `interbase` key makes an editor flag a valid config. Insert this object between `"params"` (ending `schema.json:69`) and `"sshConfig"` (`schema.json:70`):

```json
          "interbase": {
            "description": "InterBase-only connection settings. Optional",
            "type": "object",
            "additionalProperties": false,
            "properties": {
              "role": {
                "description": "SQL role activated for the attachment. Optional, 255 bytes maximum",
                "type": "string"
              },
              "connectTimeout": {
                "description": "Go duration bounding the native attachment handshake, for example 10s. Optional",
                "type": "string"
              },
              "tls": {
                "description": "Native client TLS attachment options. Requires host; encrypts the connection but does not verify server identity. Optional",
                "type": "object",
                "additionalProperties": false,
                "properties": {
                  "enabled": {
                    "description": "Required for any other tls key to be accepted",
                    "type": "boolean"
                  },
                  "serverPublicFile": {
                    "description": "Server public certificate file",
                    "type": "string"
                  },
                  "serverPublicPath": {
                    "description": "Directory searched for server public certificates",
                    "type": "string"
                  },
                  "clientCertFile": {
                    "description": "Client certificate file",
                    "type": "string"
                  },
                  "clientPassPhrase": {
                    "description": "Client certificate passphrase. Prefer clientPassPhraseFile so the passphrase is not stored in the config file",
                    "type": "string"
                  },
                  "clientPassPhraseFile": {
                    "description": "File holding the client certificate passphrase",
                    "type": "string"
                  }
                }
              }
            }
          },
```

Plan 1 adds a `dialect` property to this same `properties` object. Both are additive; if `dialect` is already present, leave it and add `interbase` beside it.

- [ ] **Step 2: Verify the JSON parses**

Run: `python3 -m json.tool schema.json > /dev/null && echo "schema.json parses"`

Expected: `schema.json parses`.

- [ ] **Step 3: Rewrite README item 2 — connection keys**

`README.md:309-313` currently reads:

```
Alternatively, supply `host`, optional `port` (default `3050`), and `path`
(or `dbName`) instead of `dataSourceName`. Without a host, the path is used as a
local attachment. `proto` may be omitted or set to `tcp` for a remote attachment.
`params.charset` defaults to `UTF8`; `WIN1250` is also supported. Built-in SSH
tunneling is not supported for this driver.
```

Replace it with:

```
Alternatively, supply `host`, optional `port` (default `3050`), and `path`
(or `dbName`) instead of `dataSourceName`. Without a host, the path is used as a
local attachment. `proto` may be omitted or set to `tcp` for a remote attachment.
`params.charset` accepts `UTF8` (the default), `WIN1250`, `WIN1252`, `ISO8859_1`
and `ASCII`. Built-in SSH tunneling is not supported for this driver.

##### interbase

Connection settings only the InterBase driver understands, nested under the
`interbase` key:

| Key            | Description                                                     |
| -------------- | --------------------------------------------------------------- |
| role           | SQL role activated for the attachment. Optional, 255 bytes max.  |
| connectTimeout | Go duration bounding the native handshake, e.g. `10s`. Optional. |
| tls            | Native client TLS attachment options. Optional.                  |

| tls key              | Description                                                |
| -------------------- | ---------------------------------------------------------- |
| enabled              | Required before any other `tls` key is accepted.           |
| serverPublicFile     | Server public certificate file.                            |
| serverPublicPath     | Directory searched for server public certificates.         |
| clientCertFile       | Client certificate file.                                   |
| clientPassPhrase     | Client certificate passphrase, stored in the config file.  |
| clientPassPhraseFile | File holding the client certificate passphrase.            |

```yaml
connections:
  - alias: centrale
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
```

`interbase.tls` requires `host` and cannot be combined with `dataSourceName`:
the driver composes the TLS attachment itself from the host and the database
path, and rejects TLS options when no host is set. `dataSourceName` keeps
working as a raw attachment string for every connection that does not use TLS.
```

If plan 1 has already retitled the section and added a `dialect` row, keep its text and place this block after it; the two do not overlap.

- [ ] **Step 4: Write README item 3 — the TLS statement**

Insert immediately after the text from Step 3:

```
**Enabling TLS does not establish server identity.** The InterBase native client
tested with this driver (`LI-V15.1.0.42`) does not verify the server hostname:
with a trusted CA it still accepted an intentionally wrong DNS name, including
through the vendor `isql`. Enabling `interbase.tls` therefore encrypts the
connection but is not proof of server identity, and sqls does not add a
verification step of its own, because a separate Go-side TLS probe would not
authenticate the native attachment. Treat the network as untrusted accordingly.
Prefer `clientPassPhraseFile` over `clientPassPhrase` so the passphrase is not
stored in `config.yml`.
```

This is the spec's §7 item 3 wording. Do not soften it, do not shorten it to "TLS is supported", and do not add a sentence suggesting a CA or a certificate makes the server trusted.

- [ ] **Step 5: Write README item 5 and retire the superseded sentences**

`README.md:322-325` currently reads:

```
Completion and hover use user table/view, column, primary-key, and foreign-key
metadata. InterBase has no schema namespace or database enumeration through this
adapter, so switching databases is not supported; configure separate connections
instead. Dialect 1 `DATE` includes both date and time. Dialect 3 is not supported.
```

Plan 1 owns the two dialect sentences in that paragraph and plan 2 owns the metadata sentence. Replace only the database-enumeration sentence — the middle one — with:

```
An InterBase connection holds a single attachment: `showDatabases` lists that
attachment string, and `switchDatabase` accepts only that same name. Configure a
separate connection entry to open another database. InterBase has no schema
namespace, so `showSchemas` reports one synthetic empty schema.
```

Then replace the TLS clause in the paragraph at `README.md:327-331`. Match on the
text below, not on the line numbers: the sentence being replaced **ends mid-line
at line 330**, and the sentence after it must survive.

Replace only this:

```
The native driver is experimental. Context cancellation cannot interrupt an
in-flight native call, and the driver exposes no TLS configuration API. Use a
trusted network or independently verified native transport security and a
least-privilege database account.
```

with:

```
The native driver is experimental. Context cancellation cannot interrupt an
in-flight native call. Use a least-privilege database account, and read the TLS
statement above before relying on `interbase.tls` for transport security.
```

**Retain the sentence that follows it verbatim**, so the paragraph still ends:

```
Executing DML/DDL uses the driver's implicit
commit behavior; SQL transaction-control statements are not supported.
```

Deleting to the end of line 330 would take half of that sentence and orphan the
rest as a dangling fragment. Step 6's grep for `commit behavior` confirms it
survived.

Leave the build, test and live-test instructions at `README.md:333-348` exactly as they are — the spec's item 6 keeps them.

- [ ] **Step 6: Check the documentation against the code**

Run:

```sh
grep -n "WIN1252\|ISO8859_1\|ASCII" README.md
grep -n "connectTimeout\|clientPassPhraseFile\|LI-V15.1.0.42" README.md
grep -n "no TLS configuration API\|Dialect 3 is not supported" README.md
grep -n "commit behavior; SQL transaction-control statements are not supported" README.md
go test ./... 2>&1 | tail -20
```

Expected: the first two commands find the new text; the third prints nothing for "no TLS configuration API" (plan 1 removes the Dialect 3 sentence, so a match there is fine if plan 1 has not landed yet); the fourth finds exactly one match, proving the Step 5 replacement did not truncate the sentence that followed it; the suite passes.

Read the TLS paragraph once more against `interbase-go/README.md:119-125` and confirm no sentence claims verification, authentication or proof of identity.

- [ ] **Step 7: Commit**

```bash
git add README.md schema.json
git commit -m "Document the InterBase connection block, its TLS caveat and single-attachment switching"
```

---

## Verification

After the last task:

```sh
go test ./... 2>&1 | tail -20
go vet ./...
CGO_ENABLED=1 go build -tags interbase ./...
CGO_ENABLED=1 go test -tags interbase ./internal/database/ -count=1 -run '^TestInterBase(CharsetAllowlistMatchesDriverNormalizer|DriverConfigIsAcceptedByConnector|NativeOpenValidatesConfigBeforeDial)$' -v
```

The live suite (`-run '^TestInterBaseLive'`) is unchanged by this plan; run it if a server is available to confirm nothing in the connect path regressed:

```sh
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```
