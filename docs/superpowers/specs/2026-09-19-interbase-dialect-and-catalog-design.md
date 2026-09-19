# InterBase dialect resolution and catalog integration — design

Date: 2026-09-19
Repository: `sqls` (fork maintained downstream of `sqls-server/sqls`)
Status: design, ready for planning
Sub-project: 2 of 3 (driver introspection → **dialect + catalog** → editor features)

## Purpose

sqls currently lexes every InterBase connection as SQL Dialect 1 while the
driver attaches every connection as SQL Dialect 3. Every Dialect 3 database is
therefore mis-lexed: `"My Column"` is tokenized as a string literal where the
server sees a delimited identifier. This sub-project makes the SQL dialect a
resolved, propagated property of a connection, replaces the hand-written
`RDB$` catalog access with the driver's `schema` package, and defines the
optional capability interfaces that sub-project 3 (Explain code action,
procedure signature help, hover DDL, definition, execution semantics) will
consume.

Three outcomes:

1. **Correctness.** The lexer, the type renderer and the catalog all agree with
   the dialect the server actually reports.
2. **Depth.** Views, procedures with parameters, generators, triggers, domains,
   indexes and external functions become first-class cached objects, and object
   DDL and query plans become available to editor features.
3. **Upstreamability.** `DBRepository` is untouched, `dialect.DialectForDriver`
   and `parser.ParseWithDriver` keep working unchanged, and every new seam is an
   additive, driver-neutral entry point. Only InterBase-specific behavior lives
   in InterBase-specific files.

## Evidence and Constraints

Every claim below was verified against the working tree at commit `b5312a2`
(sqls) and the current `interbase-go` checkout.

### Verified

| Claim | Evidence |
| --- | --- |
| `InterBaseDialect` is unconditionally Dialect 1 | `dialect/interbase.go:20-22` returns `false` from `IsDelimitedIdentifierStart` |
| sqls attaches without a dialect | `internal/database/interbase_native.go:34-39` builds `interbase.Config` with `Database`, `User`, `Password`, `Charset` only |
| The driver defaults zero to Dialect 3 | `interbase.go:372-381` `normalizeDialect`: `0, 3 -> 3`, `1 -> 1`, anything else is an error |
| The dialect seam keys only off the driver enum | `dialect/dialect.go:14-19`, `parser/parser.go:60-62`, `internal/completer/completer.go:93,118,189,193`, `internal/handler/handler.go:433-438` |
| Three hand-written catalog queries plus a numeric type switch | `internal/database/interbase_common.go:162-199`, `:327-410`, `:412-437` |
| Field type 35 is rendered `DATE` | `internal/database/interbase_common.go:357-361` |
| `CurrentDatabase`/`Databases` are inert | `internal/database/interbase_common.go:109-115` return `""` and `[]string{}`; `showDatabases` joins that empty list (`internal/handler/execute_command.go:357-367`) |
| Charset allowlist is two values | `internal/database/interbase_common.go:83-90`; the driver accepts five (`interbase.go:383-392`: `UTF8`, `WIN1250`, `WIN1252`, `ISO8859_1`, `ASCII`) |
| `Config.Host`, `Role`, `ConnectTimeout`, `TLS`, `EncryptedPassword`, `SystemEncryptionPassword`, `TransactionOptions` are unused by sqls | `interbase.go:87-103` defines them; `interbase_native.go:34-39` sets none of them |
| `DBRepository` is the shared abstraction to leave alone | `internal/database/database.go:24-36` |
| `schema` is pure Go over any `Queryer` | `schema/schema.go:16-18,42-44`; package imports are stdlib only |
| `schema` covers relations, columns, domains, procedures + parameters, indexes, constraints, triggers, generators, roles, dependencies, UDFs, privileges, and `GenerateDDL()` | `schema/README.md`, `schema/schema.go`, `schema/catalog_extended.go:15-177`, `schema/ddl.go:39-49` |
| `ErrUnsupportedDDL` cases | `schema/ddl.go:15`, `:1216-1230` (`Function`, `DatabaseFile`, `Shadow`), `:432-493` (computed columns), `:759-796` (parameter nullability, computed parameter domains, missing field source) |
| The driver's TLS caveat is real and must not be softened | `README.md:119-125`: with vendor client `LI-V15.1.0.42` a trusted CA does **not** establish server identity; an intentionally wrong DNS hostname was accepted, including through vendor `isql` |

An executable check was run to de-risk the testing strategy: `schema.Catalog`
was pointed at an in-memory SQLite fixture with quoted `RDB$...` tables inside
the untagged `internal/database` test package, and `Tables`/`Columns`/
`Domain.SQLType()` returned the expected rows. So the catalog code can be
unit-tested without cgo, without the `interbase` build tag and without a server,
exactly like `internal/database/interbase_test.go:293-418` does today.

### Corrections to the briefing

1. **`schema` is a superset of the catalog *rows*, not of the current *type
   rendering*.** `Domain.SQLType()` (`schema/ddl.go:188-346`) is Dialect 3
   correct and dialect-agnostic: it renders field type 35 as `TIMESTAMP`
   unconditionally, and it returns `ErrUnsupportedDDL` for cases the current
   sqls switch renders happily — `QUAD` (type 9), `BLOB_ID` (45), `CSTRING` (40),
   arrays (`Dimensions != 0`), and, importantly, **Dialect 1 scaled `DOUBLE`**
   (type 27 with negative scale and no numeric subtype, `ddl.go:273-276`), which
   is how Dialect 1 stores `NUMERIC`/`DECIMAL` with precision 10–18 and is
   rendered `NUMERIC(15, s)` by `interbase_common.go:348-356` today. sqls must
   therefore wrap `SQLType()` with a dialect-aware pre-pass and a fallback, not
   delegate to it blindly. Specified in §4.3.
2. **Flipping `IsDelimitedIdentifierStart` also changes single-quoted string
   rendering.** `token/lexer.go:443` derives escape preservation from
   `!IsDelimitedIdentifierStart('"')`, so a Dialect 3 InterBase dialect would
   silently start decoding `'c''d'` to `'c'd'`, and the formatter reprints
   tokens — it would corrupt user SQL. The seam needs one more additive,
   optional lexer interface. Specified in §4.2.
3. **`schema` list queries are N+1 by design.** `Catalog.Tables/Relations` runs
   one column query per relation (`schema/schema.go:360-366`),
   `Catalog.Constraints` loads the enforcing and referenced index (and each
   index loads its segments) per constraint (`catalog_extended.go:598-623`), and
   `Catalog.Procedures` loads parameters (and each parameter's domain) per
   procedure. This is the main cost of the migration and is addressed in §4.5
   and §8.
4. **TLS is not merely "nicer" through structured host/port — it is only
   possible that way.** The driver composes the attachment itself from
   `Config.Host` + `Config.Database` and rejects TLS options when `Host` is
   empty (`interbase.go:185-239`, especially `:190-193`). A hand-built
   `dataSourceName` therefore cannot carry TLS.
5. Everything else in the briefing matched the code.

### Constraints

- `DBRepository` (`internal/database/database.go:24`) does not change.
- `dialect.DialectForDriver`, `parser.ParseWithDriver`, `parser.ParseWithDialect`
  and `dialect.DataBaseKeywords`/`DataBaseFunctions` keep their signatures and
  their behavior for every non-InterBase driver.
- InterBase attach, `Diagnostics` and `Plan` require cgo and the `interbase`
  build tag (`//go:build interbase && cgo && linux && amd64`); `schema` and DDL
  generation do not.
- Services-backed administration (backup, restore, sweep, user management) is
  out of scope for the entire project and is not designed here.

## Scope

### Included

1. `DBConfig.Dialect` (`dialect`, values 0/1/3) plus config validation.
2. Dialect resolution at connect, including one-extra-attach auto-detect and an
   explicit-mismatch diagnostic.
3. A parameterized `InterBaseDialect` and a variant-aware dialect/parser/
   completer/handler seam, additive to the existing driver-keyed seam.
4. Replacement of the three hand-written `RDB$` queries and the field-type
   switch with `schema.Catalog` plus a dialect-aware type renderer.
5. Optional capability interfaces (`CatalogRepository`, `DDLRepository`,
   `ExplainRepository`) with concrete descriptor types.
6. `DBCache` extension for the new object kinds, populated only when the
   repository implements `CatalogRepository`.
7. Connection settings for structured host/port, `role`, `connectTimeout`, TLS
   and the five-charset allowlist, keeping `dataSourceName` working.
8. Meaningful `CurrentDatabase`/`Databases` for a single-attachment database and
   a coherent `switchDatabase` behavior.
9. Dialect 3 `TIMESTAMP` mapping and the Dialect 1 rules that go with it.
10. Tests and documentation for all of the above.

### Excluded (belongs to sub-project 3)

- The Explain code action, hover rendering of DDL, procedure signature help,
  definition, and execution semantics. This sub-project defines and tests the
  repository/cache contracts they call; it does not add LSP features.
- Any change to completion *content* beyond the two dialect-dependent keywords
  named in §4.2.

### Deferred out of this sub-project (explicitly, not silently)

| Item | Why deferred |
| --- | --- |
| `Config.EncryptedPassword` / `SystemEncryptionPassword` | Each adds another plaintext secret to `config.yml` for an encrypted-database workflow sqls does not otherwise support; revisit only on a concrete request. |
| `Config.TransactionOptions` (no-wait, record version, table reservation) and per-statement isolation | Execution semantics are sub-project 3's subject; exposing them here would fix an interface before that spec exists. |
| `schema` roles, dependencies and privileges in the cache | `schema` supports them, but no sub-project-3 feature consumes them; adding cache surface with no reader is dead weight. |
| Bulk (non-N+1) catalog reads | Requires new `schema` API in the driver repository, so it is a driver change, not an sqls change. Trigger condition in §8. |
| Services-backed admin commands | Ruled out for the whole project. |

## Architecture

### 4.1 Dialect resolution and auto-detect

#### Configuration

```go
// internal/database/config.go
type DBConfig struct {
	// ...existing fields...
	Dialect   int              `json:"dialect" yaml:"dialect"`
	InterBase *InterBaseConfig `json:"interbase" yaml:"interbase"`
}
```

`Dialect` accepts `0` (auto-detect), `1` and `3`. Validation:

- `c.Dialect != 0 && c.Driver != dialect.DatabaseDriverInterBase` →
  `invalid: connections[].dialect is only supported by the interbase driver`.
  Rejecting it elsewhere prevents a silently ignored setting.
- Inside the InterBase case, `c.Dialect` outside `{0, 1, 3}` →
  `invalid: connections[].dialect must be 0 (auto), 1, or 3`.

#### Connection state

```go
// internal/database/driver.go
type DBConnection struct {
	Conn    *sql.DB
	SSHConn *ssh.Client
	Tunnel  io.Closer
	Driver  dialect.DatabaseDriver

	// Variant is the server-side SQL variant resolved at connect. It is empty
	// for drivers that have no variants.
	Variant dialect.SQLVariant
	// DatabaseName identifies the attached database for drivers with a single
	// attachment per connection. Empty when the driver enumerates databases.
	DatabaseName string
	// Warnings are non-fatal connect-time diagnostics for the user.
	Warnings []string
}

// DriverVariant pairs the driver with the resolved variant. Nil-safe.
func (db *DBConnection) DriverVariant() dialect.DriverVariant
```

#### Connect flow (`interBaseOpen`, tagged file)

```
validate config
attachment, charset, role, connectTimeout, tls := from DBConfig
requested := cfg.Dialect                    // 0, 1 or 3

conn := attach(requested)                   // interbase.NewConnector + sql.OpenDB + Ping
diag, diagErr := diagnostics(conn)          // interbase.Diagnostics over a *sql.Conn

switch {
case diagErr != nil:
        resolved = requested or 3 if requested == 0
        warn("interbase: could not read database diagnostics (%v); using SQL dialect %d", …)
case requested == 0 && diag.SQLDialect == 1:
        close(conn)
        conn = attach(1)                    // the single extra attach
        resolved = 1
case requested == 0:
        resolved = 3                        // includes any unexpected reported value
        if diag.SQLDialect != 3 { warn(unexpected reported dialect …) }
case int64(requested) != diag.SQLDialect:
        resolved = requested
        warn(mismatch message below)
default:
        resolved = requested
}

return &DBConnection{
        Conn:         conn,
        Driver:       dialect.DatabaseDriverInterBase,
        Variant:      dialect.InterBaseSQLVariant(resolved),
        DatabaseName: attachment,
        Warnings:     warnings,
}
```

Exactly one extra attach, and only for an auto-detected Dialect 1 database, as
locked.

#### Explicit mismatch: warning, not failure

When the user pins `dialect: 1` or `dialect: 3` and the database reports the
other value, sqls **connects and warns**. Message text (single line, no
credentials — an attachment string carries none):

```
interbase: connection "centrale" is configured for SQL dialect 1 but the
database reports SQL dialect 3; sqls will lex and render types as dialect 1.
Remove `dialect` or set `dialect: 0` to follow the database.
```

Justification: attaching a client at a dialect different from the database's own
dialect is a supported InterBase configuration (it is the normal way to read a
Dialect 1 database from Dialect 3 tooling during a migration), the server
enforces its own rules regardless, and refusing to attach would take away a
working setup that the user asked for explicitly. The warning restores the
correctness signal without removing the choice. A diagnostics *failure* is also
never fatal: metadata introspection must not block editing.

Warnings travel on `DBConnection.Warnings`. `(*Server).reconnectionDB` logs each
warning with `log.Println`, and the two paths that hold a `*jsonrpc2.Conn`
(`handleInitialize` and `handleWorkspaceDidChangeConfiguration`) additionally
send them through the existing `lsp.Messenger.ShowWarning`
(`internal/lsp/client.go:45`). No new LSP plumbing is introduced.

#### Repository construction carries the dialect

`Factory` is `func(*sql.DB) DBRepository` (`internal/database/driver.go:16`) and
cannot see the resolved dialect. Rather than change it for every driver, add an
optional connection-aware factory:

```go
// internal/database/driver.go
type ConnFactory func(*DBConnection) DBRepository

func RegisterConnFactory(name dialect.DatabaseDriver, factory ConnFactory)
func CreateRepositoryFromConnection(conn *DBConnection) (DBRepository, error)
```

`CreateRepositoryFromConnection` prefers a registered `ConnFactory` and falls
back to the existing `*sql.DB` factory, so every other driver is untouched and
`CreateRepository` keeps working. `(*Server).newDBRepository` calls the new
function. InterBase registers both:

```go
func NewInterBaseDBRepository(conn *sql.DB) DBRepository              // unchanged: dialect 3, no database name
func NewInterBaseDBRepositoryFromConnection(conn *DBConnection) DBRepository
```

### 4.2 Dialect propagation seam

#### `dialect` package additions

```go
// SQLVariant identifies a server-side SQL variant within one driver. The empty
// variant selects the driver's default.
type SQLVariant string

const (
	SQLVariantDefault    SQLVariant = ""
	SQLVariantInterBase1 SQLVariant = "interbase-dialect-1"
	SQLVariantInterBase3 SQLVariant = "interbase-dialect-3"
)

// DriverVariant pairs a driver with the variant resolved for a connection.
type DriverVariant struct {
	Driver  DatabaseDriver
	Variant SQLVariant
}

// InterBaseSQLVariant maps an InterBase SQL dialect number to its variant.
// Zero maps to the driver default, dialect 3.
func InterBaseSQLVariant(sqlDialect int) SQLVariant

// InterBaseSQLDialect is the inverse; it returns 1 or 3.
func (v SQLVariant) InterBaseSQLDialect() int

func DialectForDriverVariant(dv DriverVariant) Dialect
func DataBaseKeywordsForVariant(dv DriverVariant) []string
func DataBaseFunctionsForVariant(dv DriverVariant) []string
```

`DialectForDriver(driver)` becomes
`DialectForDriverVariant(DriverVariant{Driver: driver})`, and
`DataBaseKeywords`/`DataBaseFunctions` delegate the same way: identical
signatures, identical results for every non-InterBase driver.

#### Parameterized `InterBaseDialect`

```go
// InterBaseDialect implements the lexical rules of one InterBase SQL dialect.
// The zero value is Dialect 3, matching the interbase-go default.
type InterBaseDialect struct {
	// SQLDialect is 1 or 3; zero is treated as 3.
	SQLDialect int
}

func (d *InterBaseDialect) IsDelimitedIdentifierStart(r rune) bool {
	return r == '"' && d.SQLDialect != 1
}

// PreservesQuotedStringEscapes keeps doubled quotes in token text for both
// dialects, because the formatter reprints tokens verbatim.
func (d *InterBaseDialect) PreservesQuotedStringEscapes() bool { return true }
```

`IsIdentifierStart`, `IsIdentifierPart` (`$` allowed after the first character),
`IsPlaceHolderStart('?')` and `MatchKeyword` are unchanged and dialect
independent. "Zero means 3" mirrors the driver's own `normalizeDialect`
convention, so a zero-value dialect and a zero-value `interbase.Config` agree.

#### Lexer: one additive optional interface

`token/lexer.go:442-444` currently derives escape preservation from the
delimited-identifier rule. Add an optional interface next to the existing
`keywordMatcher` (`token/lexer.go:69-71`):

```go
type quotedStringEscapePreserver interface {
	PreservesQuotedStringEscapes() bool
}

func (t *Tokenizer) tokenizeSingleQuotedString() string {
	preserve := !t.Dialect.IsDelimitedIdentifierStart('"')
	if p, ok := t.Dialect.(quotedStringEscapePreserver); ok {
		preserve = p.PreservesQuotedStringEscapes()
	}
	return t.tokenizeQuotedString('\'', preserve)
}
```

No dialect other than `InterBaseDialect` implements it, so nothing else changes.
Without this, switching InterBase to Dialect 3 would make the formatter rewrite
`'c''d'` as `'c'd'`.

#### Propagation

| Layer | Existing | Added / changed |
| --- | --- | --- |
| `dialect` | `DialectForDriver(driver)` | `DialectForDriverVariant(dv)` (existing delegates) |
| `parser` | `ParseWithDriver(text, driver)` | `ParseWithDriverVariant(text string, dv dialect.DriverVariant)` (existing delegates) |
| `completer` | `Completer.Driver` | new field `Variant dialect.SQLVariant`; `Complete` uses `parser.ParseWithDriverVariant`, `getLastWordWithVariant`, `DataBaseKeywordsForVariant`, `DataBaseFunctionsForVariant` |
| `handler` | `(*Server).parserDriver()` | replaced by `(*Server).parserDriverVariant() dialect.DriverVariant`; it is an unexported method of the fork's server, so nothing public breaks |
| `handler` helpers | `hoverWithDriver`, `definitionWithDriver`, `renameWithDriver`, `SignatureHelpWithDriver`, `getStatementsWithDriver`, `formatter.FormatWithDriver` | each takes `dialect.DriverVariant` instead of `dialect.DatabaseDriver`; names unchanged so sub-project 3 and existing tests keep their call shapes |
| `completion.go` | `c.Driver = s.parserDriver()` | `dv := s.parserDriverVariant(); c.Driver, c.Variant = dv.Driver, dv.Variant` |

`getLastWordWithDriver` keeps its signature and delegates to a new
`getLastWordWithVariant`.

**Offline fallback.** With no live connection, `parserDriverVariant()` returns
the zero `DriverVariant`; for the InterBase driver that resolves to Dialect 3,
matching the driver default. This is a deliberate behavior change to the
InterBase-only default and it is what fixes the bug for the common case.

**Variant-aware keywords.** `DataBaseKeywordsForVariant` returns the existing
`interbaseKeywords` for Dialect 1 and that list plus `TIME` and `TIMESTAMP` for
Dialect 3, because those two types do not exist in Dialect 1. Function lists are
identical for both variants. Nothing else in completion content changes here.

### 4.3 Catalog integration

#### Placement relative to the build tag

`schema` is pure Go (stdlib-only imports) and was verified to compile and run
inside the untagged `internal/database` test package. Catalog code therefore
lives **outside** the `interbase` build tag:

| File | Tag | Contents |
| --- | --- | --- |
| `internal/database/capability.go` | none | capability interfaces, descriptor types, sentinel errors (driver neutral) |
| `internal/database/interbase_common.go` | none | config helpers (attachment, charset, dialect, role/timeout/TLS mapping), repository struct, `DBRepository` methods |
| `internal/database/interbase_catalog.go` | none | `schema.Catalog` → descriptors, dialect-aware type rendering, `CatalogRepository` implementation |
| `internal/database/interbase_ddl.go` | none | `DDLRepository` implementation over `GenerateDDL()` |
| `internal/database/interbase_native.go` | `interbase && cgo && linux && amd64` | attach, dialect resolution via `interbase.Diagnostics`, `ExplainPlan` via `interbase.Plan` |
| `internal/database/interbase_stub.go` | inverse | `interBaseOpen` error stub (unchanged) |

Justification: keeping catalog and DDL untagged means every contributor can run
the catalog tests with plain `go test ./...` against the SQLite fixture, the
`interbase` build tag keeps meaning exactly "links the native client", and
`ExplainRepository` is *absent* rather than stubbed on untagged builds — a
capability interface should not be implemented by a method that always fails.
The cost is that the untagged `internal/database` package now imports
`interbase-go/schema`, making the replaced module a compile-time dependency of
every build; the module is already required and replaced in `go.mod`, and §5
records what upstreaming would need.

#### Repository

```go
type InterBaseDBRepository struct {
	Conn *sql.DB
	// SQLDialect is 1 or 3; zero is treated as 3.
	SQLDialect int
	// DatabaseName is the attachment string; empty when unknown.
	DatabaseName string
}
```

All `DBRepository` methods route through `schema.New(q)`:

| Method | Implementation |
| --- | --- |
| `SchemaTables` | `catalog.Relations(ctx, "")` → `map[string][]string{"": names}` (tables *and* views, as today) |
| `DescribeDatabaseTable` / `…BySchema` | `catalog.Relations(ctx, "")` for columns + `catalog.Constraints(ctx, "")` for primary-key membership |
| `DescribeForeignKeysBySchema` | `catalog.Constraints(ctx, "")` filtered to `FOREIGN KEY`, paired via `Columns`/`ReferencedColumns` |
| `CurrentDatabase` | `DatabaseName` |
| `Databases` | `[]string{DatabaseName}`, or `[]string{}` when empty |
| `CurrentSchema` / `Schemas` | unchanged: `""` and `[]string{""}` |
| `Exec` / `Query` | unchanged |

Each cache build wraps its reads in one explicit read-only transaction
(`db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})`, `schema.New(tx)`,
`defer tx.Rollback()`), which gives a single consistency boundary and avoids a
per-statement implicit transaction in the native client. `ObjectDDL` and
`ExplainPlan` are interactive one-shots and use the `*sql.DB` directly.

The three `interBase*Query` constants and `interBaseColumnRow`,
`interBaseColumnDescription`, `interBaseNullability`, `interBaseColumnType`,
`interBaseCharacterLength`, `interBaseNumericType`, `parseInterBaseForeignKeys`
are deleted. `interBaseDefault`/`interBaseEffectiveDefault` (the
`DEFAULT `-prefix stripper, `interbase_common.go:301-325`) are kept: `schema`
returns `DefaultSource` verbatim, including the keyword.

#### Dialect-aware type rendering

```go
// interBaseTypeName renders the column/parameter type for sqlDialect.
func interBaseTypeName(domain *schema.Domain, sqlDialect int) string
```

Order of rules:

1. `domain == nil` → `""` (the catalog row resolved to no `RDB$FIELDS` entry).
2. **Dialect 1 overrides**, applied before delegating:
   - field type 35 → `DATE` (Dialect 1 has no separate `TIMESTAMP`; Dialect 3
     keeps `TIMESTAMP`, which is the mapping fix);
   - field type 27 with `FieldScale < 0` → `NUMERIC(p, |scale|)`, or
     `DECIMAL(p, |scale|)` when `FieldSubType == 2`, with `p = FieldPrecision`
     when positive and `15` otherwise. This is how Dialect 1 stores
     `NUMERIC`/`DECIMAL` above precision 9, and `schema.Domain.SQLType()`
     rejects it (`ddl.go:273-276`).
3. Otherwise delegate to `domain.SQLType()`.
4. On error, fall back without inventing a declaration:
   type 9 → `QUAD`, 40 → `CSTRING(n)`, 45 → `BLOB_ID`, arrays → the renderable
   base followed by ` ARRAY`, any other failure with a valid `FieldType` →
   `TYPE(n)` (the current behavior), invalid `FieldType` → `""`.

For the one-line `ColumnDesc.Type` the rendered text is trimmed at the first
` CHARACTER SET ` or ` COLLATE `, because `SQLType()` appends both and the
completion detail line must stay short; the full text is kept for
`DomainDesc.Type` and DDL, and charset/collation are separate `DomainDesc`
fields. The cut is safe: no base type name contains those keywords.

`ColumnDesc` fields map as: `Type` from the renderer, `Null` = `"NO"` when the
column's `NullFlag` or its domain's `NullFlag` is set, `Key` = `"YES"` when the
column is a primary-key segment, `Default` from the column default falling back
to the domain default, `Extra` = `"COMPUTED"` for computed columns (previously
always empty; computed columns are common in the target database and hover
should say so), `Schema` = `""`.

### 4.4 Capability interfaces and descriptor types

`internal/database/capability.go`, driver neutral, no `schema` import in the
interface declarations. Callers type-assert; absence is the normal case.

```go
// CatalogRepository is implemented by repositories that can enumerate catalog
// objects beyond tables and columns. All methods return objects for the whole
// attachment; sqls drivers without a schema namespace use an empty Schema.
type CatalogRepository interface {
	DescribeViews(ctx context.Context) ([]*ObjectDesc, error)
	DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)
	DescribeSequences(ctx context.Context) ([]*ObjectDesc, error)
	DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error)
	DescribeDomains(ctx context.Context) ([]*DomainDesc, error)
	DescribeIndexes(ctx context.Context) ([]*IndexDesc, error)
	DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error)
}

// DDLRepository is implemented by repositories that can reproduce an object's
// definition. Callers must handle ErrObjectNotFound and ErrDDLUnsupported.
type DDLRepository interface {
	ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)
}

// ExplainRepository is implemented by repositories that can return a server
// query plan without executing the statement's result set.
type ExplainRepository interface {
	ExplainPlan(ctx context.Context, query string) (string, error)
}

var (
	// ErrObjectNotFound reports that the named catalog object does not exist.
	ErrObjectNotFound = errors.New("database: catalog object not found")
	// ErrDDLUnsupported reports that the catalog cannot reproduce the object's
	// definition faithfully. The wrapped message explains why.
	ErrDDLUnsupported = errors.New("database: DDL is unavailable for this object")
)
```

Object kinds and descriptors:

```go
// ObjectKind identifies a catalog object kind for capability lookups.
type ObjectKind string

const (
	ObjectKindTable     ObjectKind = "table"
	ObjectKindView      ObjectKind = "view"
	ObjectKindProcedure ObjectKind = "procedure"
	ObjectKindTrigger   ObjectKind = "trigger"
	ObjectKindDomain    ObjectKind = "domain"
	ObjectKindIndex     ObjectKind = "index"
	ObjectKindSequence  ObjectKind = "sequence"
	ObjectKindFunction  ObjectKind = "function"
)

// ObjectDesc describes a named catalog object that carries no attributes
// beyond identity and an optional definition. Used for views and sequences.
type ObjectDesc struct {
	Schema     string     // "" for InterBase
	Name       string
	Kind       ObjectKind
	Owner      string     // "" when the catalog has none
	Comment    string     // RDB$DESCRIPTION, "" when absent
	Definition string     // view source; "" for sequences
}

// ProcedureDesc describes a stored procedure and its ordered parameters.
type ProcedureDesc struct {
	Schema  string
	Name    string
	Comment string
	Source  string           // PSQL body verbatim, "" when unavailable
	Inputs  []*ParameterDesc // ordered by Position
	Outputs []*ParameterDesc // ordered by Position
}

// ParameterDirection identifies a parameter's direction.
type ParameterDirection string

const (
	ParameterIn  ParameterDirection = "in"
	ParameterOut ParameterDirection = "out"
)

// ParameterDesc describes one procedure or external-function parameter.
type ParameterDesc struct {
	Name      string
	Position  int
	Direction ParameterDirection
	Type      string             // rendered for the resolved dialect; "" when unrenderable
	Domain    string             // user domain name; "" for an inline type
	Nullable  string             // "YES", "NO", or "" when the catalog cannot say
	Default   sql.NullString
	Comment   string
}

// TriggerDesc describes a DML or database trigger.
type TriggerDesc struct {
	Schema   string
	Name     string
	Table    string // "" for a database-level trigger
	Event    string // e.g. "BEFORE INSERT"; "" when the type code is unknown
	Sequence int
	Enabled  bool
	Source   string
	Comment  string
}

// DomainDesc describes a user domain.
type DomainDesc struct {
	Schema    string
	Name      string
	Type      string         // full rendering, including CHARACTER SET/COLLATE
	Nullable  string         // "YES", "NO", or ""
	Default   sql.NullString
	Check     string         // RDB$VALIDATION_SOURCE verbatim, "" when absent
	Charset   string
	Collation string
	Comment   string
}

// IndexDesc describes an index and its ordered segments.
type IndexDesc struct {
	Schema     string
	Name       string
	Table      string
	Columns    []string // ordered segment names; empty for an expression index
	Expression string   // "" for a segment index
	Unique     bool
	Enabled    bool
	Constraint string // owning constraint name; "" for a standalone index
	Comment    string
}

// FunctionDesc describes an external function (UDF) declaration. sqls never
// invokes a UDF; this is declaration metadata only.
type FunctionDesc struct {
	Schema     string
	Name       string
	ReturnType string           // rendered; "" when the catalog cannot render it
	Arguments  []*ParameterDesc // Direction is always ParameterIn
	Module     string
	EntryPoint string
	Comment    string
}
```

Sequences use `ObjectDesc` because `schema.Sequence`
(`catalog_extended.go:15-19`) carries only a name, an id and a system flag —
there is nothing else to model.

#### `ObjectDDL` and the `ErrUnsupportedDDL` cases

| Kind | Source |
| --- | --- |
| `table`, `view` | `catalog.Table` / `catalog.View` (these load constraints, indexes and triggers, `schema/schema.go:261-286`) then `Relation.GenerateDDL()` |
| `procedure` | `catalog.Procedure` then `Procedure.GenerateDDL()` |
| `trigger`, `index`, `domain`, `sequence` | the matching singular getter then `GenerateDDL()` |
| `function` | always `ErrDDLUnsupported` (`schema/ddl.go:1216-1219`) |

A `nil, nil` result from a singular getter becomes `ErrObjectNotFound`. A
`schema.ErrUnsupportedDDL` becomes
`fmt.Errorf("interbase: %s: %w", detail, ErrDDLUnsupported)`, where `detail` is
the driver's explanation (for example `procedure "POST_ORDER": parameter
"QTY" nullability is unknown`). Callers use `errors.Is`.

**What sqls shows the user** (the contract sub-project 3 implements; specified
here so behavior is decided, not invented later): when `ObjectDDL` returns
`ErrDDLUnsupported`, the feature falls back to the cached descriptor rendering
that exists today — `database.TableDoc` for a table or view, a signature line
for a procedure — and appends one italic line:

```
_DDL unavailable: interbase: table "ORDERS": column "TOTAL" is computed._
```

For `ErrObjectNotFound`, the feature shows nothing rather than an error. Both
cases are expected in the target database: computed columns block table DDL, and
`schema/README.md` states that procedure parameter nullability is unknown unless
a non-nullable domain proves it, so procedure DDL will frequently fall back to
`ProcedureDesc.Source`, which is always available from the catalog.

`ExplainPlan` is implemented only in the tagged file: it acquires a `*sql.Conn`
and calls sub-project 1's `interbase.Plan(ctx, conn, query)`, returning the plan
text unchanged. On untagged builds `*InterBaseDBRepository` does not satisfy
`ExplainRepository`, and sub-project 3's code action simply does not offer.

### 4.5 Caching

```go
// CatalogCache holds extended catalog objects. A nil *CatalogCache means the
// active repository does not implement CatalogRepository. All maps are keyed by
// the upper-cased object name.
type CatalogCache struct {
	Views           map[string]*ObjectDesc
	Procedures      map[string]*ProcedureDesc
	Sequences       map[string]*ObjectDesc
	Domains         map[string]*DomainDesc
	Functions       map[string]*FunctionDesc
	Indexes         map[string]*IndexDesc
	IndexesByTable  map[string][]*IndexDesc
	Triggers        map[string]*TriggerDesc
	TriggersByTable map[string][]*TriggerDesc
}

type DBCache struct {
	// ...existing fields...
	Catalog *CatalogCache
}
```

Nil-safe accessors in the style of the existing ones (`DBCache.Column`,
`DBCache.SortedTables`):

```go
func (dc *DBCache) HasCatalog() bool
func (dc *DBCache) View(name string) (*ObjectDesc, bool)
func (dc *DBCache) Procedure(name string) (*ProcedureDesc, bool)
func (dc *DBCache) Sequence(name string) (*ObjectDesc, bool)
func (dc *DBCache) Domain(name string) (*DomainDesc, bool)
func (dc *DBCache) Function(name string) (*FunctionDesc, bool)
func (dc *DBCache) Index(name string) (*IndexDesc, bool)
func (dc *DBCache) Trigger(name string) (*TriggerDesc, bool)
func (dc *DBCache) IndexesForTable(table string) []*IndexDesc
func (dc *DBCache) TriggersForTable(table string) []*TriggerDesc
func (dc *DBCache) SortedProcedures() []string
func (dc *DBCache) SortedViews() []string
```

Generation and refresh:

```go
// GenerateCatalogCache returns nil, false, nil when the repository has no
// extended catalog.
func (u *DBCacheGenerator) GenerateCatalogCache(ctx context.Context) (*CatalogCache, bool, error)
```

`GenerateDBCachePrimary` and `GenerateDBCacheSecondary` keep their signatures.
The worker's existing update goroutine (`internal/database/worker.go:49-69`)
calls `GenerateCatalogCache` alongside `GenerateDBCacheSecondary` and swaps the
result in with a copy-on-write setter mirroring `setColumnCache`:

```go
func (w *Worker) setCatalogCache(c *CatalogCache)
```

**The extended catalog is built in the secondary (asynchronous) pass, which
`ReCache` already kicks off at connect** (`worker.go:75-82`). It is eager in the
sense the briefing requires — no user action, no lazy per-object fetch — but it
does not block `initialize`, and the primary pass keeps delivering tables and
columns at the same latency as today. That matters because §8 shows the catalog
reads are N+1. A catalog error is logged and leaves the previous
`*CatalogCache` in place, exactly as the existing secondary column pass does.

Eagerness is affordable for the target database: table metadata is about 7 KB
and procedures about 34 KB including source, so the whole extended cache is well
under 100 KB of retained descriptors.

### 4.6 Connection configuration

```go
// InterBaseConfig holds settings that only the InterBase driver understands.
type InterBaseConfig struct {
	Role           string              `json:"role" yaml:"role"`
	ConnectTimeout string              `json:"connectTimeout" yaml:"connectTimeout"` // Go duration, e.g. "10s"
	TLS            *InterBaseTLSConfig `json:"tls" yaml:"tls"`
}

type InterBaseTLSConfig struct {
	Enabled              bool   `json:"enabled" yaml:"enabled"`
	ServerPublicFile     string `json:"serverPublicFile" yaml:"serverPublicFile"`
	ServerPublicPath     string `json:"serverPublicPath" yaml:"serverPublicPath"`
	ClientCertFile       string `json:"clientCertFile" yaml:"clientCertFile"`
	ClientPassPhrase     string `json:"clientPassPhrase" yaml:"clientPassPhrase"`
	ClientPassPhraseFile string `json:"clientPassPhraseFile" yaml:"clientPassPhraseFile"`
}
```

A nested block keeps InterBase-only keys out of the shared `DBConfig` surface,
matching the existing `sshConfig` precedent; `dialect` stays top-level as
locked, because a SQL dialect number is a plausible generic concept. Duration is
a string because YAML has no duration type and unit-less integers are ambiguous.

Mapping to `interbase.Config`:

| sqls setting | driver field |
| --- | --- |
| `host` (+ `port`, default 3050) | `Host: "db.example.test/3050"` |
| `path` or `dbName` | `Database` |
| `dataSourceName` | `Database`, with `Host` empty |
| `user`, `passwd` | `User`, `Password` |
| `params.charset` | `Charset` |
| `dialect` (resolved) | `Dialect` |
| `interbase.role` | `Role` |
| `interbase.connectTimeout` | `ConnectTimeout` |
| `interbase.tls.*` | `TLS` |

This replaces sqls's hand-built `host/port:path` string
(`interbase_common.go:58-62`) with the driver's own composition
(`interbase.go:185-239`), which is also what makes TLS possible at all.
`interBaseAttachment` is retained but reduced to producing the *display*
attachment string used for `DatabaseName`, `showConnections` and error messages.

Validation added to the InterBase case:

- `interbase.tls` with a non-empty `dataSourceName` or an empty `host` →
  `invalid: connections[].interbase.tls requires connections[].host` (the driver
  rejects TLS without a host, so failing in validation gives a better message).
- TLS options set with `enabled: false` → error, mirroring the driver.
- `connectTimeout` unparseable or negative → error.
- `role` containing a NUL byte or longer than 255 bytes → error (the driver's
  own limits, checked early).
- Charset allowlist widened to `UTF8`, `WIN1250`, `WIN1252`, `ISO8859_1`,
  `ASCII`, empty meaning `UTF8` — the driver's exact set
  (`interbase.go:383-392`). sqls keeps its own copy because the driver's
  normalizer is unexported and validation must work on untagged builds; a test
  documents that the two lists must stay in sync.

**Security wording is a hard requirement.** Neither the README nor any log line
may imply that enabling TLS authenticates the server. The documented sentence
(§7) states that the vendor client tested (`LI-V15.1.0.42`) accepted a wrong DNS
hostname even with a trusted CA, that sqls performs no TLS preflight of its own
because a second connection would not authenticate the native attachment, and
that `clientPassPhraseFile` is preferred over `clientPassPhrase` so the
passphrase is not stored in `config.yml`.

### 4.7 Database identity, `showDatabases`, `switchDatabase`

`CurrentDatabase` returns the attachment string and `Databases` returns it as a
single-element list, so `showDatabases` prints the attached database instead of
nothing. The attachment string — not a prettified basename — is used because it
is exactly what the user configured and it disambiguates remote attachments;
deriving a short name from `host/port:/srv/data/x.ib` would be ambiguous.

`switchDatabase` to the current name is accepted and re-connects (harmless
refresh). Any other value returns
`interbase: this connection has a single attachment; configure another
connection to open a different database` instead of silently reconnecting to the
same database, which is what happens today.

## Compatibility and Upstreaming

**Generically contributable** (no InterBase knowledge, useful to upstream sqls):

- `dialect.SQLVariant`, `dialect.DriverVariant`, `DialectForDriverVariant`,
  `DataBaseKeywordsForVariant`, `DataBaseFunctionsForVariant`, with the existing
  functions delegating — this is the "a driver can have server-side variants"
  seam upstream lacks (it currently encodes MySQL versions as separate driver
  enums).
- `parser.ParseWithDriverVariant` and the `getLastWordWithVariant` sibling.
- The optional `quotedStringEscapePreserver` lexer interface, which fixes a real
  class of formatter corruption for any dialect that preserves doubled quotes.
- `DBConnection.Variant`, `.DatabaseName`, `.Warnings`, `ConnFactory`,
  `RegisterConnFactory`, `CreateRepositoryFromConnection` — a driver may need
  connection-level context to build its repository.
- `CatalogRepository`, `DDLRepository`, `ExplainRepository`, the descriptor
  types, `ObjectKind`, `ErrObjectNotFound`, `ErrDDLUnsupported`, `CatalogCache`
  and the `DBCache` accessors: all driver neutral. PostgreSQL and MySQL could
  implement them later without touching `DBRepository`.

**InterBase-only** (stays in this fork):

- `InterBaseDialect.SQLDialect`, `InterBaseSQLVariant`, the InterBase keyword
  delta, `interbase_common.go`, `interbase_catalog.go`, `interbase_ddl.go`,
  `interbase_native.go`, `interbase_stub.go`.
- `DBConfig.Dialect`, `DBConfig.InterBase` and their validation.

**Upstreaming caveat.** After this change the untagged `internal/database`
package imports `interbase-go/schema`, so a build needs the replaced module
present. That is already true of `go.mod` today, but it means the catalog files
cannot be upstreamed as-is until the driver module is published; the generic
items above have no such dependency and can be contributed separately.

**Behavior changes for existing users** (all InterBase-only):

1. Default lexing for an InterBase connection becomes Dialect 3 unless the
   database reports Dialect 1 or the user pins `dialect: 1`.
2. `showDatabases` prints the attachment instead of an empty string.
3. Column types for Dialect 3 databases change where they were wrong
   (`TIMESTAMP`, charset-aware `CHAR`/`VARCHAR`, `BLOB SUB_TYPE TEXT`).
4. A pinned dialect that disagrees with the database produces a warning toast.

## Testing Strategy

The repository's conventions are followed: table-driven tests in the package
under test, a SQLite in-memory fixture standing in for the `RDB$` catalog
(`internal/database/interbase_test.go:293-418`), mock repositories for
cache/handler tests (`internal/database/database_mock.go`), and gated live tests
behind both the build tag and `INTERBASE_*` environment variables
(`internal/database/interbase_live_test.go`).

### Offline unit tests (no server, no build tag, `go test ./...`)

**`dialect/interbase_test.go`** (extended; the existing Dialect-1 assertions in
`TestInterBaseDialect1Syntax` move into the table)

- `TestInterBaseDialectLexicalRulesBySQLDialect`: table over
  `{SQLDialect: 0, 1, 3}` × `{IsDelimitedIdentifierStart('"'),
  IsIdentifierStart('$'), IsIdentifierPart('$'), IsPlaceHolderStart('?'),
  PreservesQuotedStringEscapes()}`. Expected: `"` delimits identifiers for 0 and
  3 but not 1; everything else identical across dialects.
- `TestDialectForDriverVariant`: `{interbase, ""} → *InterBaseDialect{3}`,
  `{interbase, SQLVariantInterBase1} → *InterBaseDialect{1}`,
  `{interbase, SQLVariantInterBase3} → *InterBaseDialect{3}`,
  `{"mock", anything} → *GenericSQLDialect`. Plus
  `DialectForDriver(interbase)` equals the default-variant result, and
  `DialectForDriver("mock")` is unchanged.
- `TestInterBaseSQLVariantRoundTrip`: `0→3`, `1→1`, `3→3`, and
  `SQLVariant("").InterBaseSQLDialect() == 3`.
- `TestInterBaseKeywordsByVariant`: Dialect 3 keywords contain `TIMESTAMP` and
  `TIME`; Dialect 1 keywords do not; neither contains the Firebird words the
  existing test already forbids.

**`parser/parser_test.go`** — `TestParseWithInterBaseDialect1` switches to
`ParseWithDriverVariant(..., DriverVariant{interbase, SQLVariantInterBase1})`
and keeps its current expectations. A new table-driven
`TestParseInterBaseDoubleQuotedText` asserts, for the input
`SELECT "My Column", 'c''d' FROM T`:

| variant | `"My Column"` token | `'c''d'` token text |
| --- | --- | --- |
| Dialect 1 | `SingleQuotedString`, text `"My Column"` | `'c''d'` |
| Dialect 3 | `SQLKeyword` whose `*token.SQLWord` has `QuoteStyle '"'` and `Value "My Column"` | `'c''d'` |

The second column is the regression guard for the escape-preservation interface.

**`token/lexer_test.go`** — `TestTokenizeQuotedStringEscapePreservation` over
`{GenericSQLDialect, InterBaseDialect{1}, InterBaseDialect{3}}`, asserting that
only the generic dialect decodes `''`.

**`internal/database/interbase_config_test.go`**

- `TestInterBaseConfigValidatesDialect`: `0`, `1`, `3` accepted; `2`, `4`, `-1`
  rejected mentioning `dialect`; `dialect: 3` on `driver: mysql` rejected
  mentioning `interbase`.
- `TestInterBaseConfigValidatesCharset`: the five accepted values (upper and
  lower case), empty → `UTF8`, `latin1` rejected, conflicting duplicate
  `charset` params rejected.
- `TestInterBaseCharsetMatchesDriverAllowlist`: asserts the sqls allowlist
  equals the documented driver set, so the copies cannot drift silently.
- `TestInterBaseConfigValidatesConnectionOptions`: table over `role` too long /
  with NUL, `connectTimeout: "abc"`, `connectTimeout: "-1s"`,
  `tls.enabled` with `dataSourceName`, `tls` with no `host`, TLS options with
  `enabled: false`; each expects an error mentioning the offending key and
  each asserts the error does not contain `Passwd` or `ClientPassPhrase`
  (extending the existing no-secret-leak assertion at
  `interbase_test.go:258-260`).
- `TestInterBaseDriverConfigMapping`: builds `interbase.Config` values through
  the untagged mapping helper and asserts `Host`/`Database`/`Role`/
  `ConnectTimeout`/`TLS` for: local path, host+port, host with default port,
  `dataSourceName`, and TLS enabled with a server public file.

**`internal/database/interbase_catalog_test.go`** — the SQLite fixture from the
existing test is extended with the tables `schema` reads
(`RDB$RELATIONS`, `RDB$RELATION_FIELDS`, `RDB$FIELDS`, `RDB$CHARACTER_SETS`,
`RDB$COLLATIONS`, `RDB$VIEW_RELATIONS`, `RDB$RELATION_CONSTRAINTS`,
`RDB$REF_CONSTRAINTS`, `RDB$INDICES`, `RDB$INDEX_SEGMENTS`, `RDB$PROCEDURES`,
`RDB$PROCEDURE_PARAMETERS`, `RDB$TRIGGERS`, `RDB$GENERATORS`, `RDB$FUNCTIONS`,
`RDB$FUNCTION_ARGUMENTS`) in a shared helper
`openInterBaseCatalogFixture(t) *sql.DB`.

- `TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys`: the
  existing assertions, unchanged expectations except the documented type
  changes, proving the `schema` migration is behavior preserving.
- `TestInterBaseTypeRenderingByDialect`: the core table.

| field type / attributes | Dialect 1 | Dialect 3 |
| --- | --- | --- |
| 35 | `DATE` | `TIMESTAMP` |
| 12 | `DATE` | `DATE` |
| 13 | `TIME` | `TIME` |
| 8, subtype 1, scale −2, precision 9 | `NUMERIC(9, 2)` | `NUMERIC(9, 2)` |
| 27, subtype 0, scale −2 | `NUMERIC(15, 2)` | `NUMERIC(15, 2)` |
| 27, subtype 2, scale −4, precision 18 | `DECIMAL(18, 4)` | `DECIMAL(18, 4)` |
| 27, subtype 0, scale 0 | `DOUBLE PRECISION` | `DOUBLE PRECISION` |
| 14, charLength 10, charset UTF8 | `CHAR(10)` | `CHAR(10)` |
| 37, charLength 20 | `VARCHAR(20)` | `VARCHAR(20)` |
| 261, subtype 1 | `BLOB SUB_TYPE TEXT` | `BLOB SUB_TYPE TEXT` |
| 9 | `QUAD` | `QUAD` |
| 45 | `BLOB_ID` | `BLOB_ID` |
| 40, charLength 32 | `CSTRING(32)` | `CSTRING(32)` |
| 99 (unknown) | `TYPE(99)` | `TYPE(99)` |
| nil domain | `""` | `""` |

  A companion case asserts that the charset/collation suffix is present in
  `DomainDesc.Type` and absent from `ColumnDesc.Type`.
- `TestInterBaseDescribesExtendedCatalogObjects`: fixture contains one view, one
  procedure with two inputs and one output, one trigger, one generator, one
  domain, one unique index and one UDF; asserts every descriptor field that the
  fixture can determine, including parameter order, direction, `Nullable`
  `"YES"/"NO"/""`, and `Enabled` for an inactive index and trigger.
- `TestInterBaseObjectDDL`: table DDL for a plain table returns a `CREATE TABLE`
  containing the quoted name; a table with a computed column returns an error
  satisfying `errors.Is(err, ErrDDLUnsupported)` whose message names the column;
  `ObjectKindFunction` always returns `ErrDDLUnsupported`; an unknown name
  returns `ErrObjectNotFound`.
- `TestInterBaseCurrentDatabaseAndDatabases`: with a `DatabaseName` the
  repository returns it from both methods; without one, `""` and `[]string{}`
  (preserving the current assertions at `interbase_test.go:18-23`).

**`internal/database/capability_test.go`**

- `TestInterBaseRepositoryImplementsCapabilities`: compile-time
  `var _ CatalogRepository = (*InterBaseDBRepository)(nil)` and
  `var _ DDLRepository = (*InterBaseDBRepository)(nil)`, plus runtime assertions.
- `TestNonInterBaseRepositoriesDoNotImplementCapabilities`: table over the
  MySQL, PostgreSQL, SQLite3, MSSQL, H2, Vertica and `MockDBRepository`
  repositories asserting all three type assertions fail — this is the guard that
  the change stays additive for other drivers.
- `TestExplainRepositoryAbsentWithoutNativeBuild` (untagged file): asserts
  `*InterBaseDBRepository` does **not** implement `ExplainRepository`; the tagged
  counterpart in `interbase_live_test.go` asserts it does.

**`internal/database/cache_test.go`**

- `TestGenerateCatalogCacheFromCapabilityRepository`: a fake repository
  implementing `DBRepository` + `CatalogRepository` returns one of each object
  kind; asserts keys are upper-cased, `IndexesByTable`/`TriggersByTable`
  grouping, `SortedProcedures`, and that accessors find objects case
  insensitively.
- `TestGenerateCatalogCacheWithoutCapability`: `MockDBRepository` yields
  `(nil, false, nil)`; `DBCache.HasCatalog()` is false and every accessor
  returns `(nil, false)` or an empty slice without panicking on a nil
  `*CatalogCache`.
- `TestWorkerSwapsCatalogCache`: after `ReCache` with a capability repository,
  `Worker.Cache().Catalog` is non-nil and the previously returned `*DBCache`
  snapshot is unaffected (copy-on-write, mirroring the existing
  `setColumnCache` contract).

**`internal/handler/interbase_test.go`**

- The existing formatting test keeps its Dialect 1 expectation but now sets
  `dbConn = &database.DBConnection{Driver: interbase, Variant:
  SQLVariantInterBase1}`.
- `TestInterBaseDialect3LanguageServerFormatting`: same input with
  `SQLVariantInterBase3` formats `"literal"` as a delimited identifier and still
  emits `'c''d'` unchanged.
- `TestInterBaseVariantReachesCompletionAndHover`: with a Dialect 3 connection,
  completion for `select "My Col` treats the text as an identifier prefix; with
  Dialect 1 it does not.
- `TestParserVariantDefaultsToDialect3WithoutConnection`: `dbConn == nil` yields
  a zero `DriverVariant`, and parsing an InterBase document offline uses
  Dialect 3.

**`internal/completer/completer_test.go`** — existing InterBase cases set
`c.Variant` explicitly; a new case asserts `TIMESTAMP` is offered for Dialect 3
and not for Dialect 1.

### Gated live tests (`//go:build interbase && cgo && linux && amd64`, skip unless `INTERBASE_DATABASE`, `INTERBASE_USER`, `INTERBASE_PASSWORD` are set)

In `internal/database/interbase_live_test.go`, all read-only, no fixtures
created, following the existing skip pattern at `:22-28`:

- `TestInterBaseLiveDialectAutoDetect`: opens with `Dialect: 0` and asserts the
  resolved `DBConnection.Variant` matches `interbase.Diagnostics(...).SQLDialect`
  for whichever dialect the server under test has. Documented as the test that
  must be run against both a Dialect 1 and a Dialect 3 database; the harness
  reads the expected value from the optional `INTERBASE_EXPECT_DIALECT`
  environment variable when set, otherwise it self-checks against diagnostics.
- `TestInterBaseLiveExplicitDialectMismatchWarnsAndConnects`: opens with the
  dialect opposite to the reported one; asserts the connection succeeds, the
  variant equals the configured dialect, and `Warnings` contains one message
  naming both dialects.
- `TestInterBaseLiveCatalogObjectsSurface`: builds the primary and catalog
  caches through `DBCacheGenerator`, asserts tables and columns are non-empty,
  asserts `Catalog` is non-nil, and logs counts plus elapsed time for each pass
  (the measurement that feeds the §8 trigger).
- `TestInterBaseLiveObjectDDL`: for the first table, `ObjectDDL` either returns
  text starting with `CREATE TABLE` or an error satisfying
  `errors.Is(err, ErrDDLUnsupported)`; the same for the first procedure; a
  deliberately unknown name returns `ErrObjectNotFound`.
- `TestInterBaseLiveExplainPlan`: `ExplainPlan(ctx, "SELECT RDB$RELATION_ID FROM
  RDB$DATABASE")` returns non-empty text and does not execute a result set.
- The existing `TestInterBaseLiveReadOnlyCatalog` and
  `TestInterBaseNativeOpenValidatesConfigBeforeDial` stay.

Commands, unchanged from the README:

```shell
go test ./...
CGO_ENABLED=1 go test -tags interbase ./...
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```

## Migration and Documentation

### What changes for an existing `config.yml`

Nothing is required. Every existing InterBase connection keeps working:
`dataSourceName`, `host`/`port`/`path`/`dbName`, `user`, `passwd` and
`params.charset` are unchanged, and the new `dialect` key defaults to `0`
(auto-detect).

What an existing user will notice:

- A Dialect 3 database is now lexed correctly. Users who had worked around the
  bug by avoiding double-quoted identifiers can stop.
- A Dialect 1 database behaves as before, after one extra attach at connect.
- Pinning `dialect:` is only needed to override auto-detection.

Newly available:

```yaml
connections:
  - alias: centrale
    driver: interbase
    host: db.example.test
    port: 3050
    path: /srv/interbase/centrale.ib
    user: sqls_reader
    passwd: "your-password"
    dialect: 0            # 0 auto-detect (default), 1, or 3
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

`schema.json` gains `dialect` (integer, enum `0, 1, 3`) and the `interbase`
object with the same keys and descriptions.

### What the README must say

The `#### InterBase (SQL Dialect 1)` section (README.md:290-348) is retitled
`#### InterBase` and rewritten to cover:

1. **Dialects.** Both Dialect 1 and Dialect 3 are supported. `dialect` accepts
   `0` (default, auto-detect from the database), `1` or `3`. Auto-detect costs
   one extra attach only for a Dialect 1 database. A pinned dialect that
   disagrees with the database connects and warns. Under Dialect 1 double quotes
   delimit strings and `DATE` carries a time component; under Dialect 3 double
   quotes delimit identifiers and `TIMESTAMP` is distinct from `DATE`. In both
   dialects, unquoted identifiers may contain `$`, positional parameters use
   `?`, and doubled quotes are preserved verbatim by the formatter.
2. **Connection keys.** The keys table gains `dialect` and `interbase`, with a
   sub-table for `role`, `connectTimeout`, `tls.*`. `dataSourceName` still works
   as a raw attachment string, and the note that it cannot be combined with TLS.
   `params.charset` accepts `UTF8` (default), `WIN1250`, `WIN1252`,
   `ISO8859_1`, `ASCII`.
3. **TLS, stated without softening.** Verbatim intent: *"The InterBase native
   client tested with this driver (`LI-V15.1.0.42`) does not verify the server
   hostname: with a trusted CA it still accepted an intentionally wrong DNS
   name, including through the vendor `isql`. Enabling `interbase.tls` therefore
   encrypts the connection but is not proof of server identity, and sqls does
   not add a verification step of its own, because a separate Go-side TLS probe
   would not authenticate the native attachment. Treat the network as untrusted
   accordingly. Prefer `clientPassPhraseFile` over `clientPassPhrase` so the
   passphrase is not stored in `config.yml`."*
4. **Metadata depth.** Completion and hover use tables, views, columns, primary
   and foreign keys; the cache also holds procedures with parameters, triggers,
   generators, domains, indexes and external-function declarations, refreshed by
   the background worker. Object DDL is reproduced from the catalog and is
   unavailable for external functions, database files, shadows, tables with
   computed columns and procedures whose parameter nullability the catalog does
   not record; in those cases sqls shows the cached summary and says why the DDL
   is missing.
5. **Single attachment.** `showDatabases` lists the attached database and
   `switchDatabase` only accepts that name; use a separate connection entry for
   another database.
6. The build, test and live-test instructions stay as they are.

The stale sentence *"The driver always uses client SQL Dialect 1; no dialect
parameter is necessary"* (README.md:315) and *"Dialect 3 is not supported"*
(README.md:325) are removed, and the line about the driver exposing no TLS
configuration API (README.md:328) is replaced by item 3.

## Risks

1. **N+1 catalog reads (highest).** `Relations` costs 1 + one query per
   relation; `Constraints` costs 1 + roughly four per constraint (enforcing
   index, its segments, referenced index, its segments); `Procedures` costs 1 +
   one per procedure plus one per parameter domain. For a catalog with `R`
   relations, `K` constraints and `P` procedures the primary pass is roughly
   `1 + R + 4K` round trips and the catalog pass adds roughly `P + parameters +
   indexes + triggers`. Mitigations in this design: one read-only transaction
   per cache build, and moving the extended catalog to the asynchronous
   secondary pass so only the primary pass affects startup. **Trigger for
   escalation:** if the live measurement on the `centrale` database shows the
   primary pass above 5 seconds or the catalog pass above 30 seconds, the fix is
   a bulk projection added to the driver's `schema` package (a driver change,
   deferred out of this sub-project, not a re-introduction of hand-written
   queries in sqls).
2. **Type-rendering regressions.** Moving from a nine-case switch to
   `Domain.SQLType()` plus overrides changes strings users see. The dialect
   table test pins every case that the current code handles, plus the cases the
   current code gets wrong; anything not in that table renders `TYPE(n)` rather
   than a guess.
3. **DDL frequently unavailable.** Computed columns block table DDL and unknown
   parameter nullability blocks procedure DDL, both realistic in the target
   database. Mitigated by the specified fallback: cached descriptor rendering
   plus a one-line reason, and `ProcedureDesc.Source` is always available.
4. **Auto-detect cost on a Dialect 1 database.** One extra attach per connect,
   accepted as the locked design; it is bounded and only affects Dialect 1.
5. **Untagged dependency on the driver module.** Every sqls build now compiles
   `interbase-go/schema`. `go.mod` already requires and replaces the module, so
   this is a fork-local constraint, recorded in §5 as an upstreaming blocker for
   the catalog files only.
6. **Sub-project 1 dependency.** `ExplainPlan` and dialect resolution need
   `interbase.Diagnostics` and `interbase.Plan`. If sub-project 1's helper
   signatures land differently, only `interbase_native.go` changes; the
   capability interfaces, descriptors and cache do not.
7. **Cross-dialect test coverage needs two databases.** The live dialect tests
   are meaningful only if run against both a Dialect 1 and a Dialect 3 database.
   The offline table-driven tests cover the lexing and rendering differences
   without a server, so the live suite verifies resolution and wiring rather
   than dialect semantics.
