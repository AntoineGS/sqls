# InterBase Dialect Resolution and Propagation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the InterBase SQL dialect at connect time (auto-detected from the server, overridable by config) and propagate it through the lexer, parser, completer, formatter and handler, fixing the live bug where every Dialect 3 database is mis-lexed as Dialect 1.

**Architecture:** A new `dialect.SQLVariant`/`dialect.DriverVariant` pair names a server-side SQL variant within one driver. `InterBaseDialect` gains an `SQLDialect` field (zero means 3, matching the driver). Two additive *optional* lexer interfaces — `quotedStringEscapePreserver` and `delimitedIdentifierScanner` — let InterBase turn on delimited identifiers without the formatter corrupting `'c''d'` and without truncating `"My Column"` at the space. Every existing entry point keeps its name and its `dialect.DatabaseDriver` parameter type and gains a `…Variant` sibling; the server's own call sites move to the siblings. At connect, `interBaseOpen` attaches, reads `interbase.Diagnostics`, and re-attaches at Dialect 1 only when auto-detect finds a Dialect 1 database. The resolved variant, the attachment name and any warnings ride on `DBConnection`, and a new optional `ConnFactory` lets the InterBase repository see them.

**Tech Stack:** Go 1.23, `database/sql`, the sqls `dialect`/`token`/`parser` packages, `jsonrpc2` LSP handlers, `interbase-go` (cgo, build tag `interbase && cgo && linux && amd64`).

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-dialect-and-catalog-design.md`

This plan implements **plan 1 of 3** from that spec's "Plan decomposition" section: §4.1 in full, §4.2 in full, and README item 1. It does **not** touch §4.3/§4.4/§4.5 (catalog migration — plan 2) or §4.6/§4.7 (connection configuration and database identity — plan 3).

## Global Constraints

Values copied verbatim from the spec. Every task's requirements implicitly include this section.

- **Baseline is branch `master`.** The spec verified every claim against commit `b5312a2`; the code citations below were re-verified against the current working tree at planning time.
- `DBRepository` (`internal/database/database.go:24`) does not change.
- `dialect.DialectForDriver`, `parser.ParseWithDriver`, `parser.ParseWithDialect` and `dialect.DataBaseKeywords`/`DataBaseFunctions` keep their signatures and their behavior for every non-InterBase driver.
- **Every existing exported and unexported entry point keeps its name *and* its parameter types:** `DialectForDriver`, `DataBaseKeywords`, `DataBaseFunctions`, `ParseWithDriver`, `getLastWordWithDriver`, `getStatementsWithDriver`, `hoverWithDriver`, `definitionWithDriver`, `renameWithDriver`, `SignatureHelpWithDriver`, `formatter.FormatWithDriver`, `(*Server).parserDriver()` and `completer.Completer.Driver`. Each gains a variant-aware sibling rather than a changed signature. A later plan and a peer sub-project have call sites that depend on this.
- `(*Server).parserDriver()` survives with its current signature and becomes `return s.parserDriverVariant().Driver`.
- **`Dialect` accepts `0` (auto-detect), `1` and `3`.** Validation: `c.Dialect != 0 && c.Driver != dialect.DatabaseDriverInterBase` → `invalid: connections[].dialect is only supported by the interbase driver`. Inside the InterBase case, `c.Dialect` outside `{0, 1, 3}` → `invalid: connections[].dialect must be 0 (auto), 1, or 3`.
- **"Zero means 3"** for `InterBaseDialect.SQLDialect`, for `dialect.InterBaseSQLVariant`, and for `SQLVariant("").InterBaseSQLDialect()`. This mirrors the driver's own `normalizeDialect` (`interbase.go:372-381`: `0, 3 -> 3`, `1 -> 1`).
- **Auto-detect costs exactly one extra attach, and only for a Dialect 1 database.** Configured zero means attach with the driver default, read `Diagnostics.SQLDialect`, and re-attach with `Dialect: 1` only if the database reports 1.
- **Explicit-dialect mismatch is a warning, not a failure.** Attaching a client at a dialect different from the database's own is a supported InterBase configuration. A diagnostics *failure* is also never fatal: metadata introspection must not block editing.
- **Offline fallback.** With no live connection, `parserDriverVariant()` returns the zero `DriverVariant`; for the InterBase driver that resolves to Dialect 3, matching the driver default.
- **Mismatch warning text**, single line, no credentials — an attachment string carries none. Verbatim from the spec:

  ```
  interbase: connection "centrale" is configured for SQL dialect 1 but the
  database reports SQL dialect 3; sqls will lex and render types as dialect 1.
  Remove `dialect` or set `dialect: 0` to follow the database.
  ```

  The spec wraps it for readability; the emitted message is one line.
- Warnings travel on `DBConnection.Warnings`. `(*Server).reconnectionDB` logs each warning with `log.Println`, and the two paths that hold a `*jsonrpc2.Conn` (`handleInitialize`, `handler.go:175-176`, and `handleWorkspaceDidChangeConfiguration`, `handler.go:323-324`) additionally send them through the existing `lsp.Messenger.ShowWarning` (`internal/lsp/client.go:45`). No new LSP plumbing is introduced. `execute_command.go:394,462` also call `reconnectionDB` without a messenger, where warnings are only logged.
- **Variant-aware keywords.** `DataBaseKeywordsForVariant` returns the existing `interbaseKeywords` for Dialect 1 and that list plus `TIME` and `TIMESTAMP` for Dialect 3, because those two types do not exist in Dialect 1. Function lists are identical for both variants. Nothing else in completion content changes here.
- `IsIdentifierStart`, `IsIdentifierPart` (`$` allowed after the first character), `IsPlaceHolderStart('?')` and `MatchKeyword` are unchanged and dialect independent.
- **No dialect other than `InterBaseDialect` implements the new optional lexer interfaces**, so nothing else changes.
- InterBase attach and `Diagnostics` require cgo and the `interbase` build tag (`//go:build interbase && cgo && linux && amd64`).
- `CreateRepositoryFromConnection` prefers a registered `ConnFactory` and falls back to the existing `*sql.DB` factory, so every other driver is untouched and `CreateRepository` keeps working. **It takes the driver as an explicit argument rather than reading `DBConnection.Driver`**, because `openPostgreSQL` (`postgresql.go:55`), `openSQLite3` (`sqlite3.go:24`) and the `"mock"` opener (`database_mock.go:549`) all leave that field empty; keying the lookup off it returns `driver not found` for PostgreSQL, SQLite3 and every handler test.
- Services-backed administration (backup, restore, sweep, user management) is out of scope for the entire project.

### Execution order: 1 → 2 → 3 → 4 → 5 → 6 → **9** → 7 → 8 → 10 → 11 → 12 → 13 → 14

Task 9 is executed **after Task 6 and before Task 7**, out of numerical order. Task 7's `parserDriverVariant()` calls `DBConnection.DriverVariant()` and Task 8's tests construct `DBConnection{Variant: …}` — both arrive in Task 9, and Task 9 depends on nothing from Tasks 7 or 8. The numbering is left as written so that the cross-references throughout this document stay valid; only the order of execution changes. Tasks 7 and 9 each restate this at the point where it matters.

### Dependency: `interbase.Diagnostics` is not yet implemented

Task 12 consumes `interbase.Diagnostics(ctx context.Context, conn *sql.Conn) (interbase.DatabaseDiagnostics, error)` from the driver-side pooled-introspection work. Its **spec and plan are complete but the code is not written**:

- Spec: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/specs/2026-09-19-pooled-introspection-design.md`
- Plan: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/plans/2026-09-19-pooled-introspection.md`

The shape is final: `DatabaseDiagnostics.SQLDialect` is an `int64` decoded from `InfoDatabaseSQLDialect`, and the spec states it "is the server's value, not an echo of `Config.Dialect`, and consumers should treat it as the authoritative answer for dialect auto-detection". A nil `*sql.Conn` returns `interbase.ErrNotInterBaseConn`.

**Consequence for execution order:** Tasks 1–11 and 13–14 have no such dependency and can be completed first. Task 12 is the only task that will not compile until the driver work lands, and it lives entirely behind the `interbase` build tag, so `go test ./...` stays green regardless. The *decision logic* Task 12 wires up is implemented and unit-tested untagged in Task 11, so only the plumbing waits.

### Build and test commands

```shell
go test ./...                                   # the whole offline suite; must be green at every task boundary
CGO_ENABLED=1 go build -tags interbase ./...    # compiles the tagged InterBase adapter
CGO_ENABLED=1 go test -tags interbase ./...     # the native suite
make test                                       # build, then go test -v ./...  (Makefile:37)
```

The driver lives at `/home/a.simard@multidev.local/gits/interbase-go` via `replace interbase-go => ../interbase-go` in `go.mod`.

Live InterBase tests skip unless `INTERBASE_DATABASE`, `INTERBASE_USER` and `INTERBASE_PASSWORD` are set:

```shell
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```

### Committing

Other agents are active in this repository. **Never use `git add -A` or `git add .`** — every commit step below names its files explicitly. If `git add` or `git commit` reports `index.lock` exists, wait a moment and retry once.

---

## File Structure

**Created:**

- `dialect/variant.go` — the driver-neutral variant seam: `SQLVariant`, its constants, `DriverVariant`, `InterBaseSQLVariant`, `(SQLVariant).InterBaseSQLDialect`, `DialectForDriverVariant`, `DataBaseKeywordsForVariant`, `DataBaseFunctionsForVariant`. This is generically contributable upstream, so it is kept apart from the InterBase-specific file and from the 470-line `dialect/keyword.go` word tables.
- `dialect/variant_test.go` — tests for everything in `variant.go`.
- `internal/database/interbase_dialect.go` (no build tag) — `resolveInterBaseDialect`, the pure decision function for the connect flow, plus the warning message builders. Untagged so the branch table is unit-testable with plain `go test ./...`; the tagged file does the I/O and calls this.
- `internal/database/interbase_dialect_test.go` (no build tag) — the decision table.

**Modified:**

- `token/lexer.go` — two optional interfaces next to `keywordMatcher` (`:69-71`); `tokenizeSingleQuotedString` (`:442-444`) and `tokenizeDelimitedIdentifier` (`:489-516`) consult them.
- `dialect/interbase.go` — `InterBaseDialect` gains `SQLDialect`; `IsDelimitedIdentifierStart` (`:20-22`) becomes dialect-dependent; the two optional-interface methods are added; `interbaseDialect3Keywords` is derived from `interbaseKeywords`.
- `dialect/dialect.go:14-19` — `DialectForDriver` delegates to `DialectForDriverVariant`.
- `dialect/keyword.go:396,427` — `DataBaseKeywords`/`DataBaseFunctions` delegate to the `…ForVariant` forms.
- `parser/parser.go:60-62` — `ParseWithDriverVariant` added; `ParseWithDriver` delegates.
- `internal/completer/completer.go` — `Completer.Variant` field (`:74`); `Complete` uses the variant forms (`:93`, `:118`, `:189`, `:193`); `getLastWordWithVariant` added (`:438`).
- `internal/formatter/formatter.go:20-22` — `FormatWithDriverVariant` added; `FormatWithDriver` delegates.
- `internal/handler/handler.go` — `parserDriverVariant()` added next to `parserDriver()` (`:433-438`); `newDBRepository` (`:386-395`) uses `CreateRepositoryFromConnection`; `reconnectionDB` (`:341-359`) logs warnings; `handleInitialize` (`:175-176`) and `handleWorkspaceDidChangeConfiguration` (`:323-324`) show them.
- `internal/handler/hover.go:37,51`, `definition.go:34,41`, `rename.go:33,44`, `signature_help.go:32,43`, `execute_command.go:151,498`, `completion.go:29`, `format.go:28` — `…WithDriverVariant` siblings; the server's call sites move to them.
- `internal/database/driver.go` — `DBConnection` gains `Variant`, `DatabaseName`, `Warnings` and `DriverVariant()`; `ConnFactory`, `RegisterConnFactory`, `CreateRepositoryFromConnection` added.
- `internal/database/config.go:20-33,142-154` — `DBConfig.Dialect` and its validation.
- `internal/database/interbase_common.go:15-18,91-98` — registers the `ConnFactory`; `InterBaseDBRepository` gains `SQLDialect` and `DatabaseName`.
- `internal/database/interbase_native.go` (tagged) — the connect flow.
- `README.md:290-325` — item 1 of the documentation rewrite.

**Test files modified:** `token/lexer_test.go`, `dialect/interbase_test.go`, `parser/parser_test.go`, `internal/completer/completer_test.go`, `internal/formatter/formatter_test.go`, `internal/handler/interbase_test.go`, `internal/database/interbase_test.go`, `internal/database/interbase_live_test.go`.

### A correction to the spec, discovered by prototyping

Spec §4.2 names **one** new optional lexer interface, `quotedStringEscapePreserver`. That is necessary but **not sufficient**. `tokenizeDelimitedIdentifier` (`token/lexer.go:489-516`) breaks out of its scan loop as soon as the next rune is a space, and has no concept of a doubled quote inside an identifier. So with only the spec's one interface, Dialect 3 lexes `SELECT "My Column"` as three tokens — `"My`, ` `, `Column` — and `"a""b"` as two separate `SQLWord`s. That directly contradicts the spec's own testing table in "Offline unit tests", which requires Dialect 3 to produce one `SQLKeyword` whose `*token.SQLWord` has `QuoteStyle '"'` and `Value "My Column"`.

This plan therefore adds a **second** optional interface, `delimitedIdentifierScanner`, built to exactly the same additive pattern. It was prototyped against a scratch copy of the repository: with both interfaces present and no dialect implementing them, the entire offline suite passes unchanged, and with `InterBaseDialect` implementing both, `"My Column"`, `"a""b"` and `'c''d'` all lex and re-render correctly under both dialects. Task 1 pins the unchanged-by-default property; Task 2 pins the InterBase behavior.

---

## Task 1: Optional lexer interfaces, implemented by nothing yet

Add both optional interfaces and have the tokenizer consult them, with no dialect implementing either. Behavior for every existing dialect is byte-for-byte unchanged; the tests use a local test dialect to prove the hooks work. Splitting this from Task 2 means a reviewer can approve the lexer seam without judging the InterBase dialect change, and the suite is green at the boundary.

**Files:**
- Modify: `token/lexer.go:69-71` (add interfaces), `token/lexer.go:442-444` (`tokenizeSingleQuotedString`), `token/lexer.go:489-516` (`tokenizeDelimitedIdentifier`)
- Test: `token/lexer_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type quotedStringEscapePreserver interface { PreservesQuotedStringEscapes() bool }` (unexported, package `token`)
  - `type delimitedIdentifierScanner interface { ScansWholeDelimitedIdentifier() bool }` (unexported, package `token`)
  - A `dialect.Dialect` implementation may satisfy either, both, or neither. Not satisfying an interface preserves today's behavior exactly.

- [ ] **Step 1: Write the failing test**

Append to `token/lexer_test.go`:

```go
// optionalHookDialect is a generic dialect that opts into the optional lexer
// interfaces, so the hooks can be tested without depending on any real dialect.
type optionalHookDialect struct {
	dialect.GenericSQLDialect
	preserve  bool
	scanWhole bool
}

func (d *optionalHookDialect) PreservesQuotedStringEscapes() bool  { return d.preserve }
func (d *optionalHookDialect) ScansWholeDelimitedIdentifier() bool { return d.scanWhole }

func TestTokenizerOptionalQuotedStringEscapePreserver(t *testing.T) {
	tests := []struct {
		name     string
		dialect  dialect.Dialect
		src      string
		want     string
	}{
		{
			name:    "generic dialect decodes the doubled quote",
			dialect: &dialect.GenericSQLDialect{},
			src:     `'c''d'`,
			want:    `'c'd'`,
		},
		{
			name:    "opting out matches the generic dialect",
			dialect: &optionalHookDialect{preserve: false},
			src:     `'c''d'`,
			want:    `'c'd'`,
		},
		{
			name:    "opting in preserves the source spelling",
			dialect: &optionalHookDialect{preserve: true},
			src:     `'c''d'`,
			want:    `'c''d'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := NewTokenizer(bytes.NewBufferString(tt.src), tt.dialect).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != 1 {
				t.Fatalf("got %d tokens, want 1: %s", len(tokens), pp.Sprint(tokens))
			}
			if got, want := tokens[0].Kind, SingleQuotedString; got != want {
				t.Fatalf("token kind = %v, want %v", got, want)
			}
			if got := tokens[0].Value; got != tt.want {
				t.Fatalf("token value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTokenizerOptionalDelimitedIdentifierScanner(t *testing.T) {
	tests := []struct {
		name       string
		scanWhole  bool
		src        string
		wantValues []string
		wantQuote  rune
	}{
		{
			name:       "opting out stops at a space, as today",
			scanWhole:  false,
			src:        `"My Column"`,
			wantValues: []string{`"My`, "Column", `"`},
			wantQuote:  0,
		},
		{
			name:       "opting in scans the whole identifier",
			scanWhole:  true,
			src:        `"My Column"`,
			wantValues: []string{"My Column"},
			wantQuote:  '"',
		},
		{
			name:       "opting in keeps a doubled quote in the identifier",
			scanWhole:  true,
			src:        `"a""b"`,
			wantValues: []string{`a""b`},
			wantQuote:  '"',
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &optionalHookDialect{scanWhole: tt.scanWhole}
			tokens, err := NewTokenizer(bytes.NewBufferString(tt.src), d).Tokenize()
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, tok := range tokens {
				if tok.Kind == Whitespace {
					continue
				}
				word, ok := tok.Value.(*SQLWord)
				if !ok {
					t.Fatalf("token value = %T, want *SQLWord: %s", tok.Value, pp.Sprint(tokens))
				}
				got = append(got, word.Value)
			}
			if !cmp.Equal(got, tt.wantValues) {
				t.Fatalf("identifier values = %#v, want %#v", got, tt.wantValues)
			}

			first, ok := tokens[0].Value.(*SQLWord)
			if !ok {
				t.Fatalf("first token value = %T, want *SQLWord", tokens[0].Value)
			}
			if got, want := first.QuoteStyle, tt.wantQuote; got != want {
				t.Fatalf("first token quote style = %q, want %q", got, want)
			}
		})
	}
}

func TestTokenizerUnclosedDelimitedIdentifierStopsAtNewline(t *testing.T) {
	d := &optionalHookDialect{scanWhole: true}
	tokens, err := NewTokenizer(bytes.NewBufferString("\"abc\nfrom"), d).Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	word, ok := tokens[0].Value.(*SQLWord)
	if !ok {
		t.Fatalf("token value = %T, want *SQLWord", tokens[0].Value)
	}
	if got, want := word.Value, `"abc`; got != want {
		t.Fatalf("unclosed identifier value = %q, want %q", got, want)
	}
	if got, want := word.QuoteStyle, rune(0); got != want {
		t.Fatalf("unclosed identifier quote style = %q, want %q", got, want)
	}
	if got, want := tokens[1].Value, "\n"; got != want {
		t.Fatalf("token after unclosed identifier = %#v, want %q", tokens[1].Value, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./token/ -run 'TestTokenizerOptional|TestTokenizerUnclosed' -v`

Expected: FAIL to compile — `d.preserve undefined` is not the issue; the failure is that `optionalHookDialect`'s methods are never called, so `TestTokenizerOptionalQuotedStringEscapePreserver/opting_in_preserves_the_source_spelling` reports `token value = "'c'd'", want "'c''d'"` and both `opting in` delimited-identifier cases report the split values.

- [ ] **Step 3: Add the two optional interfaces**

In `token/lexer.go`, replace:

```go
type keywordMatcher interface {
	MatchKeyword(string) dialect.KeywordKind
}
```

with:

```go
type keywordMatcher interface {
	MatchKeyword(string) dialect.KeywordKind
}

// quotedStringEscapePreserver is an optional dialect interface. A dialect that
// implements it decides directly whether a doubled quote inside a single-quoted
// string keeps its source spelling, instead of having that derived from the
// delimited-identifier rule. Dialects that reprint tokens verbatim — as the
// formatter does — must preserve the spelling or they rewrite 'c''d' as 'c'd'.
type quotedStringEscapePreserver interface {
	PreservesQuotedStringEscapes() bool
}

// delimitedIdentifierScanner is an optional dialect interface. A dialect that
// returns true scans a delimited identifier to its closing quote rather than
// stopping at the first space, and treats a doubled closing quote as an escaped
// quote within the identifier. An unclosed identifier still ends at a newline.
type delimitedIdentifierScanner interface {
	ScansWholeDelimitedIdentifier() bool
}
```

- [ ] **Step 4: Consult the escape-preservation interface**

In `token/lexer.go`, replace:

```go
func (t *Tokenizer) tokenizeSingleQuotedString() string {
	return t.tokenizeQuotedString('\'', !t.Dialect.IsDelimitedIdentifierStart('"'))
}
```

with:

```go
func (t *Tokenizer) tokenizeSingleQuotedString() string {
	preserve := !t.Dialect.IsDelimitedIdentifierStart('"')
	if p, ok := t.Dialect.(quotedStringEscapePreserver); ok {
		preserve = p.PreservesQuotedStringEscapes()
	}
	return t.tokenizeQuotedString('\'', preserve)
}
```

- [ ] **Step 5: Consult the delimited-identifier interface**

In `token/lexer.go`, inside `tokenizeDelimitedIdentifier`, replace:

```go
	var s []rune
	for {
		n := t.Scanner.Next()
		if n == scanner.EOF {
			break
		}
		if n == end {
			isClosed = true
			break
		}
		s = append(s, n)
		if t.Scanner.Peek() == ' ' {
			break
		}
	}
```

with:

```go
	scanWhole := false
	if sc, ok := t.Dialect.(delimitedIdentifierScanner); ok {
		scanWhole = sc.ScansWholeDelimitedIdentifier()
	}

	var s []rune
	for {
		n := t.Scanner.Peek()
		if n == scanner.EOF || (scanWhole && n == '\n') {
			break
		}
		t.Scanner.Next()
		if n == end {
			if scanWhole && t.Scanner.Peek() == end {
				// A doubled quote is an escaped quote inside the identifier.
				// Keep the source spelling so the formatter reprints it verbatim.
				t.Scanner.Next()
				s = append(s, end, end)
				continue
			}
			isClosed = true
			break
		}
		s = append(s, n)
		if !scanWhole && t.Scanner.Peek() == ' ' {
			break
		}
	}
```

`Peek` followed by `Next` is exactly equivalent to `Next` when `scanWhole` is false, so the opt-out path is unchanged.

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./token/ -run 'TestTokenizerOptional|TestTokenizerUnclosed' -v`
Expected: PASS, all nine subtests.

- [ ] **Step 7: Run the whole offline suite to verify nothing regressed**

Run: `go test ./...`
Expected: every package `ok`. In particular `token`, `parser`, `internal/formatter`, `internal/handler` and `internal/completer` must be unchanged, because no shipped dialect implements either interface yet.

- [ ] **Step 8: Commit**

```bash
git add token/lexer.go token/lexer_test.go
git commit -m "feat(token): add optional lexer interfaces for escape and identifier scanning"
```

---

## Task 2: Parameterize `InterBaseDialect` by SQL dialect

Give `InterBaseDialect` an `SQLDialect` field, make `IsDelimitedIdentifierStart` depend on it, and implement both optional lexer interfaces. The zero value becomes Dialect 3, so the three existing tests that construct `&InterBaseDialect{}` and assert Dialect 1 behavior are updated in this same task — that is what keeps the boundary green.

**Files:**
- Modify: `dialect/interbase.go:1-30` (struct and lexical rules), `dialect/interbase.go:232` (keyword delta)
- Modify: `dialect/interbase_test.go:5-34`
- Modify: `token/lexer_test.go:910-912`
- Modify: `parser/parser_test.go:12-14`

**Interfaces:**
- Consumes: `quotedStringEscapePreserver`, `delimitedIdentifierScanner` (Task 1, package `token`, satisfied structurally).
- Produces:
  - `type InterBaseDialect struct { SQLDialect int }` — `SQLDialect` is 1 or 3; zero is treated as 3.
  - `func (d *InterBaseDialect) IsDelimitedIdentifierStart(r rune) bool` — `r == '"' && d.SQLDialect != 1`
  - `func (d *InterBaseDialect) PreservesQuotedStringEscapes() bool` — always `true`
  - `func (d *InterBaseDialect) ScansWholeDelimitedIdentifier() bool` — always `true`
  - `var interbaseDialect3Keywords []string` (unexported) — `interbaseKeywords` plus `TIME` and `TIMESTAMP`, sorted.

- [ ] **Step 1: Write the failing test**

In `dialect/interbase_test.go`, replace `TestInterBaseDialect1Syntax` (lines 5-23) with:

```go
func TestInterBaseDialectLexicalRulesBySQLDialect(t *testing.T) {
	tests := []struct {
		name                  string
		sqlDialect            int
		wantDelimitedIdent    bool
	}{
		{name: "zero means dialect 3", sqlDialect: 0, wantDelimitedIdent: true},
		{name: "dialect 1", sqlDialect: 1, wantDelimitedIdent: false},
		{name: "dialect 3", sqlDialect: 3, wantDelimitedIdent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &InterBaseDialect{SQLDialect: tt.sqlDialect}

			if got := d.IsDelimitedIdentifierStart('"'); got != tt.wantDelimitedIdent {
				t.Errorf("IsDelimitedIdentifierStart('\"') = %v, want %v", got, tt.wantDelimitedIdent)
			}
			if d.IsDelimitedIdentifierStart('`') {
				t.Error("InterBase never delimits identifiers with a back quote")
			}

			// Everything below is dialect independent.
			if !d.IsIdentifierPart('$') {
				t.Error("InterBase identifiers should allow '$' after the first character")
			}
			if d.IsIdentifierStart('$') {
				t.Error("'$' should not start an InterBase identifier")
			}
			if !d.IsIdentifierStart('a') || !d.IsIdentifierStart('Z') {
				t.Error("InterBase identifiers should start with a letter")
			}
			if !d.IsPlaceHolderStart('?') {
				t.Error("InterBase should accept positional '?' placeholders")
			}
			if d.IsPlaceHolderStart('$') {
				t.Error("InterBase should not use '$' as a placeholder")
			}
			if d.IsPlaceHolderPart('1') {
				t.Error("InterBase placeholders have no parts")
			}
			if got := d.MatchKeyword("GENERATOR"); got != Matched {
				t.Errorf("MatchKeyword(GENERATOR) = %v, want Matched", got)
			}
			if !d.PreservesQuotedStringEscapes() {
				t.Error("both InterBase dialects must preserve doubled quotes verbatim")
			}
			if !d.ScansWholeDelimitedIdentifier() {
				t.Error("both InterBase dialects must scan a delimited identifier whole")
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./dialect/ -run TestInterBaseDialectLexicalRulesBySQLDialect -v`
Expected: FAIL to build — `unknown field SQLDialect in struct literal`, `d.PreservesQuotedStringEscapes undefined`, `d.ScansWholeDelimitedIdentifier undefined`.

- [ ] **Step 3: Parameterize the dialect**

In `dialect/interbase.go`, replace lines 3-7 and 20-22. First the type:

```go
// InterBaseDialect implements the lexical rules of one InterBase SQL dialect.
// Dialect 1 uses double quotes for string literals; Dialect 3 uses them for
// delimited identifiers. Both permit '$' in regular identifiers (for example,
// RDB$DATABASE) and use positional '?' placeholders.
//
// The zero value is Dialect 3, matching the interbase-go default: the driver's
// normalizeDialect maps a zero Config.Dialect to 3, so a zero-value dialect and
// a zero-value interbase.Config agree.
type InterBaseDialect struct {
	// SQLDialect is 1 or 3; zero is treated as 3.
	SQLDialect int
}
```

Then the delimited-identifier rule:

```go
func (d *InterBaseDialect) IsDelimitedIdentifierStart(r rune) bool {
	return r == '"' && d.SQLDialect != 1
}

// PreservesQuotedStringEscapes keeps doubled quotes in token text for both
// dialects, because the formatter reprints tokens verbatim. Without it, turning
// on delimited identifiers for Dialect 3 would make the formatter rewrite
// 'c''d' as 'c'd' and corrupt user SQL.
func (d *InterBaseDialect) PreservesQuotedStringEscapes() bool { return true }

// ScansWholeDelimitedIdentifier keeps `"My Column"` a single identifier token
// and keeps a doubled quote inside one. Dialect 1 never reaches the delimited
// identifier path, so returning true unconditionally is safe for both.
func (d *InterBaseDialect) ScansWholeDelimitedIdentifier() bool { return true }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./dialect/ -run TestInterBaseDialectLexicalRulesBySQLDialect -v`
Expected: PASS, three subtests.

- [ ] **Step 5: Run the whole suite and observe exactly three known failures**

Run: `go test ./...`

Expected: FAIL in three places, each because the test constructs a zero-value `InterBaseDialect` and asserts Dialect 1 behavior, which is now Dialect 3:

- `dialect`: `TestDatabaseDriverInterBasePlumbing` still passes; nothing else in `dialect` fails.
- `token`: `TestTokenizer_InterBaseDialect1` — `token/lexer_test.go:912` uses `&dialect.InterBaseDialect{}`.
- `parser`: `TestParseWithInterBaseDialect1` — `parser/parser_test.go:14` uses `ParseWithDriver(input, dialect.DatabaseDriverInterBase)`.

`internal/formatter`, `internal/handler`, `internal/completer` and `internal/database` must all be `ok`. If any of those fails, stop: the change has a wider blast radius than expected.

- [ ] **Step 6: Pin the two lexer tests to explicit dialects**

In `token/lexer_test.go`, change line 912 from `&dialect.InterBaseDialect{}` to `&dialect.InterBaseDialect{SQLDialect: 1}`, and append a Dialect 3 companion:

```go
func TestTokenizer_InterBaseDialect3(t *testing.T) {
	src := `"a""b" 'c''d' RDB$DATABASE ?`
	tokenizer := NewTokenizer(bytes.NewBufferString(src), &dialect.InterBaseDialect{SQLDialect: 3})

	tokens, err := tokenizer.Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 7 {
		t.Fatalf("got %d tokens, want 7: %s", len(tokens), pp.Sprint(tokens))
	}

	word, ok := tokens[0].Value.(*SQLWord)
	if !ok {
		t.Fatalf("double-quoted Dialect 3 token value = %T, want *SQLWord", tokens[0].Value)
	}
	if got, want := tokens[0].Kind, SQLKeyword; got != want {
		t.Errorf("double-quoted Dialect 3 token kind = %v, want %v", got, want)
	}
	if got, want := word.Value, `a""b`; got != want {
		t.Errorf("delimited identifier value = %q, want %q", got, want)
	}
	if got, want := word.QuoteStyle, '"'; got != want {
		t.Errorf("delimited identifier quote style = %q, want %q", got, want)
	}
	if got, want := word.String(), `"a""b"`; got != want {
		t.Errorf("delimited identifier reprint = %q, want %q", got, want)
	}
	if got, want := tokens[0].To, (Pos{Line: 0, Col: 6}); got != want {
		t.Errorf("delimited identifier end = %v, want %v", got, want)
	}

	// The escape-preservation guard: a single-quoted string must survive
	// unchanged under Dialect 3 even though '"' now delimits identifiers.
	if got, want := tokens[2].Kind, SingleQuotedString; got != want {
		t.Errorf("single-quoted Dialect 3 string kind = %v, want %v", got, want)
	}
	if got, want := tokens[2].Value, `'c''d'`; got != want {
		t.Errorf("single-quoted Dialect 3 string value = %q, want %q", got, want)
	}

	catalog, ok := tokens[4].Value.(*SQLWord)
	if !ok {
		t.Fatalf("RDB$DATABASE token value = %T, want *SQLWord", tokens[4].Value)
	}
	if got, want := catalog.Value, "RDB$DATABASE"; got != want {
		t.Errorf("RDB$DATABASE value = %q, want %q", got, want)
	}
	if got, want := tokens[6].Value, "?"; got != want {
		t.Errorf("positional placeholder value = %q, want %q", got, want)
	}
}

// TestTokenizeQuotedStringEscapePreservation is the spec's named regression
// guard: only the generic dialect decodes a doubled quote. Both InterBase
// dialects keep the source spelling, because the formatter reprints tokens.
func TestTokenizeQuotedStringEscapePreservation(t *testing.T) {
	tests := []struct {
		name    string
		dialect dialect.Dialect
		want    string
	}{
		{name: "generic decodes", dialect: &dialect.GenericSQLDialect{}, want: `'c'd'`},
		{name: "interbase dialect 1 preserves", dialect: &dialect.InterBaseDialect{SQLDialect: 1}, want: `'c''d'`},
		{name: "interbase dialect 3 preserves", dialect: &dialect.InterBaseDialect{SQLDialect: 3}, want: `'c''d'`},
		{name: "interbase zero value preserves", dialect: &dialect.InterBaseDialect{}, want: `'c''d'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := NewTokenizer(bytes.NewBufferString(`'c''d'`), tt.dialect).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != 1 {
				t.Fatalf("got %d tokens, want 1: %s", len(tokens), pp.Sprint(tokens))
			}
			if got := tokens[0].Value; got != tt.want {
				t.Fatalf("token value = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 7: Pin the parser test to an explicit dialect and add the Dialect 3 case**

In `parser/parser_test.go`, change line 14 from:

```go
	parsed, err := ParseWithDriver(input, dialect.DatabaseDriverInterBase)
```

to:

```go
	parsed, err := ParseWithDialect(input, &dialect.InterBaseDialect{SQLDialect: 1})
```

The rest of `TestParseWithInterBaseDialect1` is unchanged and its expectations still hold. Task 4 switches this line again to `ParseWithDriverVariant` once that function exists; using `ParseWithDialect` here keeps this task self-contained.

Then append:

```go
func TestParseInterBaseDoubleQuotedText(t *testing.T) {
	const input = `SELECT "My Column", 'c''d' FROM T`

	tests := []struct {
		name            string
		sqlDialect      int
		wantQuotedKind  token.Kind
		wantQuotedText  string
		wantQuoteStyle  rune
	}{
		{
			name:           "dialect 1 lexes double quotes as a string",
			sqlDialect:     1,
			wantQuotedKind: token.SingleQuotedString,
			wantQuotedText: `"My Column"`,
			wantQuoteStyle: 0,
		},
		{
			name:           "dialect 3 lexes double quotes as a delimited identifier",
			sqlDialect:     3,
			wantQuotedKind: token.SQLKeyword,
			wantQuotedText: "My Column",
			wantQuoteStyle: '"',
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseWithDialect(input, &dialect.InterBaseDialect{SQLDialect: tt.sqlDialect})
			if err != nil {
				t.Fatal(err)
			}

			var sqlTokens []*ast.SQLToken
			collectSQLTokens(parsed, &sqlTokens)

			var quoted *ast.SQLToken
			var escaped string
			for _, sqlToken := range sqlTokens {
				if word, ok := sqlToken.Value.(*token.SQLWord); ok && word.Value == tt.wantQuotedText {
					quoted = sqlToken
					continue
				}
				if sqlToken.Kind == token.SingleQuotedString {
					text, _ := sqlToken.Value.(string)
					if text == `"My Column"` {
						quoted = sqlToken
						continue
					}
					escaped = text
				}
			}

			if quoted == nil {
				t.Fatalf("no token matched %q in %q", tt.wantQuotedText, input)
			}
			if got, want := quoted.Kind, tt.wantQuotedKind; got != want {
				t.Errorf("double-quoted token kind = %v, want %v", got, want)
			}
			if word, ok := quoted.Value.(*token.SQLWord); ok {
				if got, want := word.QuoteStyle, tt.wantQuoteStyle; got != want {
					t.Errorf("double-quoted token quote style = %q, want %q", got, want)
				}
				if got, want := word.Value, tt.wantQuotedText; got != want {
					t.Errorf("double-quoted token value = %q, want %q", got, want)
				}
			}

			// The regression guard for the escape-preservation interface: this
			// must hold for BOTH dialects, or the formatter corrupts user SQL.
			if got, want := escaped, `'c''d'`; got != want {
				t.Errorf("single-quoted token text = %q, want %q", got, want)
			}
			if got := parsed.String(); got != input {
				t.Errorf("round trip String() = %q, want %q", got, input)
			}
		})
	}
}
```

- [ ] **Step 8: Run the whole suite to verify it is green**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 9: Commit**

```bash
git add dialect/interbase.go dialect/interbase_test.go token/lexer_test.go parser/parser_test.go
git commit -m "feat(dialect): parameterize InterBaseDialect by SQL dialect number"
```

---

## Task 3: The `SQLVariant`/`DriverVariant` seam

Add the driver-neutral variant vocabulary and the three variant-aware lookups, with the existing driver-keyed functions delegating to them. This is the generically-upstreamable core of §4.2.

**Files:**
- Create: `dialect/variant.go`
- Create: `dialect/variant_test.go`
- Modify: `dialect/dialect.go:14-19`
- Modify: `dialect/keyword.go:396-425`, `dialect/keyword.go:427-456`
- Modify: `dialect/interbase.go` (add `interbaseDialect3Keywords`)
- Modify: `dialect/interbase_test.go:36-66`

**Interfaces:**
- Consumes: `InterBaseDialect{SQLDialect int}` (Task 2).
- Produces:
  - `type SQLVariant string` with `SQLVariantDefault SQLVariant = ""`, `SQLVariantInterBase1 SQLVariant = "interbase-dialect-1"`, `SQLVariantInterBase3 SQLVariant = "interbase-dialect-3"`
  - `type DriverVariant struct { Driver DatabaseDriver; Variant SQLVariant }`
  - `func InterBaseSQLVariant(sqlDialect int) SQLVariant`
  - `func (v SQLVariant) InterBaseSQLDialect() int`
  - `func DialectForDriverVariant(dv DriverVariant) Dialect`
  - `func DataBaseKeywordsForVariant(dv DriverVariant) []string`
  - `func DataBaseFunctionsForVariant(dv DriverVariant) []string`
  - `DialectForDriver`, `DataBaseKeywords`, `DataBaseFunctions` keep their exact signatures and delegate.

The spec lists `TestDialectForDriverVariant`, `TestInterBaseSQLVariantRoundTrip` and `TestInterBaseKeywordsByVariant` under `dialect/interbase_test.go`. They live in the new `dialect/variant_test.go` instead: same package, same assertions, but next to the code they test. `TestInterBaseDialectLexicalRulesBySQLDialect` stays in `interbase_test.go` as the spec places it, because it tests the InterBase dialect itself.

- [ ] **Step 1: Write the failing test**

Create `dialect/variant_test.go`:

```go
package dialect

import (
	"reflect"
	"testing"
)

func TestInterBaseSQLVariantRoundTrip(t *testing.T) {
	tests := []struct {
		sqlDialect  int
		wantVariant SQLVariant
		wantBack    int
	}{
		{sqlDialect: 0, wantVariant: SQLVariantInterBase3, wantBack: 3},
		{sqlDialect: 1, wantVariant: SQLVariantInterBase1, wantBack: 1},
		{sqlDialect: 3, wantVariant: SQLVariantInterBase3, wantBack: 3},
	}

	for _, tt := range tests {
		got := InterBaseSQLVariant(tt.sqlDialect)
		if got != tt.wantVariant {
			t.Errorf("InterBaseSQLVariant(%d) = %q, want %q", tt.sqlDialect, got, tt.wantVariant)
		}
		if back := got.InterBaseSQLDialect(); back != tt.wantBack {
			t.Errorf("InterBaseSQLVariant(%d).InterBaseSQLDialect() = %d, want %d", tt.sqlDialect, back, tt.wantBack)
		}
	}

	if got, want := SQLVariantDefault.InterBaseSQLDialect(), 3; got != want {
		t.Errorf("SQLVariantDefault.InterBaseSQLDialect() = %d, want %d", got, want)
	}
	if got, want := SQLVariant("nonsense").InterBaseSQLDialect(), 3; got != want {
		t.Errorf("unknown variant InterBaseSQLDialect() = %d, want %d", got, want)
	}
}

func TestDialectForDriverVariant(t *testing.T) {
	tests := []struct {
		name           string
		dv             DriverVariant
		wantSQLDialect int
		wantGeneric    bool
	}{
		{
			name:           "interbase default variant is dialect 3",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase},
			wantSQLDialect: 3,
		},
		{
			name:           "interbase dialect 1 variant",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1},
			wantSQLDialect: 1,
		},
		{
			name:           "interbase dialect 3 variant",
			dv:             DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3},
			wantSQLDialect: 3,
		},
		{
			name:        "unknown driver stays generic",
			dv:          DriverVariant{Driver: DatabaseDriver("mock"), Variant: SQLVariantInterBase1},
			wantGeneric: true,
		},
		{
			name:        "postgresql stays generic",
			dv:          DriverVariant{Driver: DatabaseDriverPostgreSQL},
			wantGeneric: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DialectForDriverVariant(tt.dv)
			if tt.wantGeneric {
				if _, ok := got.(*GenericSQLDialect); !ok {
					t.Fatalf("DialectForDriverVariant(%#v) = %T, want *GenericSQLDialect", tt.dv, got)
				}
				return
			}
			ib, ok := got.(*InterBaseDialect)
			if !ok {
				t.Fatalf("DialectForDriverVariant(%#v) = %T, want *InterBaseDialect", tt.dv, got)
			}
			if ib.SQLDialect != tt.wantSQLDialect {
				t.Fatalf("resolved SQLDialect = %d, want %d", ib.SQLDialect, tt.wantSQLDialect)
			}
		})
	}
}

func TestDialectForDriverDelegatesToDefaultVariant(t *testing.T) {
	ib, ok := DialectForDriver(DatabaseDriverInterBase).(*InterBaseDialect)
	if !ok {
		t.Fatalf("DialectForDriver(interbase) = %T, want *InterBaseDialect", DialectForDriver(DatabaseDriverInterBase))
	}
	if got, want := ib.SQLDialect, 3; got != want {
		t.Errorf("DialectForDriver(interbase).SQLDialect = %d, want %d", got, want)
	}
	if _, ok := DialectForDriver(DatabaseDriver("mock")).(*GenericSQLDialect); !ok {
		t.Error("unknown drivers should retain generic parsing")
	}
}

func TestInterBaseKeywordsByVariant(t *testing.T) {
	dialect1 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1})
	dialect3 := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3})
	defaultVariant := DataBaseKeywordsForVariant(DriverVariant{Driver: DatabaseDriverInterBase})

	for _, word := range []string{"TIME", "TIMESTAMP"} {
		if containsString(dialect1, word) {
			t.Errorf("Dialect 1 keywords must not contain %q; that type does not exist in Dialect 1", word)
		}
		if !containsString(dialect3, word) {
			t.Errorf("Dialect 3 keywords must contain %q", word)
		}
	}
	for _, word := range []string{"SELECT", "GENERATOR", "ROWS", "DATE"} {
		if !containsString(dialect1, word) || !containsString(dialect3, word) {
			t.Errorf("both InterBase dialects must contain %q", word)
		}
	}
	for _, unsupported := range []string{"FIRST", "SKIP", "AUTONOMOUS", "WITH_LOCK"} {
		if containsString(dialect1, unsupported) || containsString(dialect3, unsupported) {
			t.Errorf("InterBase completion must not suggest Firebird syntax %q", unsupported)
		}
	}
	if !reflect.DeepEqual(defaultVariant, dialect3) {
		t.Error("the default InterBase variant must use the Dialect 3 keyword list")
	}
	if len(dialect3) != len(dialect1)+2 {
		t.Errorf("Dialect 3 keywords = %d words, want %d (Dialect 1 plus TIME and TIMESTAMP)", len(dialect3), len(dialect1)+2)
	}

	// Function lists are identical for both variants.
	fn1 := DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase1})
	fn3 := DataBaseFunctionsForVariant(DriverVariant{Driver: DatabaseDriverInterBase, Variant: SQLVariantInterBase3})
	if !reflect.DeepEqual(fn1, fn3) {
		t.Error("InterBase function lists must be identical for both dialects")
	}
}

func TestVariantLookupsAreUnchangedForOtherDrivers(t *testing.T) {
	drivers := []DatabaseDriver{
		DatabaseDriverMySQL, DatabaseDriverMySQL8, DatabaseDriverMySQL57, DatabaseDriverMySQL56,
		DatabaseDriverPostgreSQL, DatabaseDriverSQLite3, DatabaseDriverMssql, DatabaseDriverOracle,
		DatabaseDriverH2, DatabaseDriverVertica, DatabaseDriverClickhouse, DatabaseDriver("mock"),
	}
	for _, driver := range drivers {
		dv := DriverVariant{Driver: driver, Variant: SQLVariantInterBase1}
		if got, want := DataBaseKeywordsForVariant(dv), DataBaseKeywords(driver); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a variant must not change the keyword list", driver)
		}
		if got, want := DataBaseFunctionsForVariant(dv), DataBaseFunctions(driver); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: a variant must not change the function list", driver)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./dialect/ -run 'Variant' -v`
Expected: FAIL to build — `undefined: SQLVariant`, `undefined: DriverVariant`, `undefined: InterBaseSQLVariant`, `undefined: DialectForDriverVariant`, `undefined: DataBaseKeywordsForVariant`, `undefined: DataBaseFunctionsForVariant`.

- [ ] **Step 3: Create `dialect/variant.go`**

```go
package dialect

// SQLVariant identifies a server-side SQL variant within one driver. The empty
// variant selects the driver's default. It exists because a single driver can
// speak more than one dialect of its server's SQL — InterBase SQL Dialect 1 and
// Dialect 3 differ in whether a double quote delimits a string or an identifier.
type SQLVariant string

const (
	SQLVariantDefault    SQLVariant = ""
	SQLVariantInterBase1 SQLVariant = "interbase-dialect-1"
	SQLVariantInterBase3 SQLVariant = "interbase-dialect-3"
)

// DriverVariant pairs a driver with the variant resolved for a connection.
// The zero value selects the driver's default variant.
type DriverVariant struct {
	Driver  DatabaseDriver
	Variant SQLVariant
}

// InterBaseSQLVariant maps an InterBase SQL dialect number to its variant.
// Zero maps to the driver default, dialect 3.
func InterBaseSQLVariant(sqlDialect int) SQLVariant {
	if sqlDialect == 1 {
		return SQLVariantInterBase1
	}
	return SQLVariantInterBase3
}

// InterBaseSQLDialect is the inverse of InterBaseSQLVariant; it returns 1 or 3.
// Every variant other than SQLVariantInterBase1 — including the default and any
// unrecognized value — resolves to 3, matching the driver's normalizeDialect.
func (v SQLVariant) InterBaseSQLDialect() int {
	if v == SQLVariantInterBase1 {
		return 1
	}
	return 3
}

// DialectForDriverVariant returns the lexical rules for a driver and variant.
func DialectForDriverVariant(dv DriverVariant) Dialect {
	if dv.Driver == DatabaseDriverInterBase {
		return &InterBaseDialect{SQLDialect: dv.Variant.InterBaseSQLDialect()}
	}
	return &GenericSQLDialect{}
}

// DataBaseKeywordsForVariant returns the completion keyword list for a driver
// and variant. Only InterBase distinguishes variants today.
func DataBaseKeywordsForVariant(dv DriverVariant) []string {
	if dv.Driver == DatabaseDriverInterBase && dv.Variant.InterBaseSQLDialect() == 1 {
		return interbaseKeywords
	}
	return dataBaseKeywords(dv.Driver)
}

// DataBaseFunctionsForVariant returns the completion function list for a driver
// and variant. No driver distinguishes variants today; the seam exists so a
// caller can pass a DriverVariant uniformly.
func DataBaseFunctionsForVariant(dv DriverVariant) []string {
	return dataBaseFunctions(dv.Driver)
}
```

- [ ] **Step 4: Add the Dialect 3 keyword delta**

In `dialect/interbase.go`, add `"sort"` to the imports (the file currently has none, so add an import block after `package dialect`), and append after the `interbaseKeywords` slice (which ends at line 232):

```go
// interbaseDialect3Keywords is interbaseKeywords plus the two types that exist
// only in SQL Dialect 3. Deriving it from the Dialect 1 list keeps one source
// of truth, so a word added to interbaseKeywords reaches both dialects.
var interbaseDialect3Keywords = func() []string {
	words := make([]string, 0, len(interbaseKeywords)+2)
	words = append(words, interbaseKeywords...)
	words = append(words, "TIME", "TIMESTAMP")
	sort.Strings(words)
	return words
}()
```

- [ ] **Step 5: Make the driver-keyed lookups delegate**

In `dialect/dialect.go`, replace:

```go
func DialectForDriver(driver DatabaseDriver) Dialect {
	if driver == DatabaseDriverInterBase {
		return &InterBaseDialect{}
	}
	return &GenericSQLDialect{}
}
```

with:

```go
// DialectForDriver returns the lexical rules for a driver's default variant.
// It is retained with its exact signature; callers that have resolved a
// server-side variant should use DialectForDriverVariant instead.
func DialectForDriver(driver DatabaseDriver) Dialect {
	return DialectForDriverVariant(DriverVariant{Driver: driver})
}
```

In `dialect/keyword.go`, rename the two existing functions to unexported helpers and add delegating wrappers. Change line 396 from `func DataBaseKeywords(driver DatabaseDriver) []string {` to `func dataBaseKeywords(driver DatabaseDriver) []string {`, and line 427 from `func DataBaseFunctions(driver DatabaseDriver) []string {` to `func dataBaseFunctions(driver DatabaseDriver) []string {`. Inside `dataBaseKeywords`, change the InterBase case from `return interbaseKeywords` to `return interbaseDialect3Keywords` — the driver's default variant is Dialect 3. Then insert immediately before `func dataBaseKeywords`:

```go
// DataBaseKeywords returns the completion keyword list for a driver's default
// variant. It is retained with its exact signature; callers that have resolved
// a server-side variant should use DataBaseKeywordsForVariant instead.
func DataBaseKeywords(driver DatabaseDriver) []string {
	return DataBaseKeywordsForVariant(DriverVariant{Driver: driver})
}

// DataBaseFunctions returns the completion function list for a driver's default
// variant. See DataBaseFunctionsForVariant.
func DataBaseFunctions(driver DatabaseDriver) []string {
	return DataBaseFunctionsForVariant(DriverVariant{Driver: driver})
}
```

- [ ] **Step 6: Update the existing keyword test for the new default**

In `dialect/interbase_test.go`, `TestInterBaseKeywordsAndFunctions` (line 36) asserts on `DataBaseKeywords(DatabaseDriverInterBase)`, which now returns the Dialect 3 list. Its existing assertions — `SELECT`, `ROWS`, `GENERATOR` present; `FIRST`, `SKIP`, `AUTONOMOUS`, `WITH_LOCK` absent — all still hold, so the test needs no edit. Confirm this by reading it rather than assuming.

- [ ] **Step 7: Run the dialect tests to verify they pass**

Run: `go test ./dialect/ -v`
Expected: PASS for every test, including `TestInterBaseSQLVariantRoundTrip`, `TestDialectForDriverVariant`, `TestDialectForDriverDelegatesToDefaultVariant`, `TestInterBaseKeywordsByVariant`, `TestVariantLookupsAreUnchangedForOtherDrivers`, and the pre-existing `TestInterBaseKeywordsAndFunctions` and `TestDatabaseDriverInterBasePlumbing`.

- [ ] **Step 8: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 9: Commit**

```bash
git add dialect/variant.go dialect/variant_test.go dialect/dialect.go dialect/keyword.go dialect/interbase.go dialect/interbase_test.go
git commit -m "feat(dialect): add SQLVariant and DriverVariant seam with delegating lookups"
```

---

## Task 4: `parser.ParseWithDriverVariant`

**Files:**
- Modify: `parser/parser.go:60-62`
- Modify: `parser/parser_test.go`

**Interfaces:**
- Consumes: `dialect.DriverVariant`, `dialect.DialectForDriverVariant` (Task 3).
- Produces: `func ParseWithDriverVariant(text string, dv dialect.DriverVariant) (ast.TokenList, error)`. `ParseWithDriver(text string, driver dialect.DatabaseDriver)` keeps its signature and delegates.

- [ ] **Step 1: Write the failing test**

Append to `parser/parser_test.go`:

```go
func TestParseWithDriverVariant(t *testing.T) {
	const input = `SELECT "My Column" FROM T`

	dialect1, err := ParseWithDriverVariant(input, dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dialect3, err := ParseWithDriverVariant(input, dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if quotedTokenKind(t, dialect1) != token.SingleQuotedString {
		t.Error("Dialect 1 must lex a double-quoted word as a string")
	}
	if quotedTokenKind(t, dialect3) != token.SQLKeyword {
		t.Error("Dialect 3 must lex a double-quoted word as a delimited identifier")
	}

	// The default variant resolves to Dialect 3, and ParseWithDriver agrees.
	defaultVariant, err := ParseWithDriverVariant(input, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if quotedTokenKind(t, defaultVariant) != token.SQLKeyword {
		t.Error("the default InterBase variant must lex as Dialect 3")
	}
	viaDriver, err := ParseWithDriver(input, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal(err)
	}
	if quotedTokenKind(t, viaDriver) != quotedTokenKind(t, defaultVariant) {
		t.Error("ParseWithDriver must match the default-variant result")
	}
}

func quotedTokenKind(t *testing.T, parsed ast.TokenList) token.Kind {
	t.Helper()
	var sqlTokens []*ast.SQLToken
	collectSQLTokens(parsed, &sqlTokens)
	for _, sqlToken := range sqlTokens {
		if word, ok := sqlToken.Value.(*token.SQLWord); ok && word.Value == "My Column" {
			return sqlToken.Kind
		}
		if text, ok := sqlToken.Value.(string); ok && text == `"My Column"` {
			return sqlToken.Kind
		}
	}
	t.Fatalf("no token matched the double-quoted text")
	return token.ILLEGAL
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./parser/ -run TestParseWithDriverVariant -v`
Expected: FAIL to build — `undefined: ParseWithDriverVariant`.

- [ ] **Step 3: Add the sibling and delegate**

In `parser/parser.go`, replace:

```go
func ParseWithDriver(text string, driver dialect.DatabaseDriver) (ast.TokenList, error) {
	return ParseWithDialect(text, dialect.DialectForDriver(driver))
}
```

with:

```go
// ParseWithDriver parses text using a driver's default variant. It is retained
// with its exact signature; callers that have resolved a server-side variant
// should use ParseWithDriverVariant instead.
func ParseWithDriver(text string, driver dialect.DatabaseDriver) (ast.TokenList, error) {
	return ParseWithDriverVariant(text, dialect.DriverVariant{Driver: driver})
}

// ParseWithDriverVariant parses text using the lexical rules of a driver and
// its resolved server-side SQL variant.
func ParseWithDriverVariant(text string, dv dialect.DriverVariant) (ast.TokenList, error) {
	return ParseWithDialect(text, dialect.DialectForDriverVariant(dv))
}
```

- [ ] **Step 4: Switch the Dialect 1 test to the new entry point**

In `parser/parser_test.go`, `TestParseWithInterBaseDialect1` line 14 currently reads `ParseWithDialect(input, &dialect.InterBaseDialect{SQLDialect: 1})` after Task 2. Change it to exercise the seam the spec names:

```go
	parsed, err := ParseWithDriverVariant(input, dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	})
```

- [ ] **Step 5: Run the parser tests to verify they pass**

Run: `go test ./parser/... -v`
Expected: PASS, including `TestParseWithInterBaseDialect1`, `TestParseInterBaseDoubleQuotedText` and `TestParseWithDriverVariant`.

- [ ] **Step 6: Commit**

```bash
git add parser/parser.go parser/parser_test.go
git commit -m "feat(parser): add ParseWithDriverVariant sibling"
```

---

## Task 5: Completer carries the variant

**Files:**
- Modify: `internal/completer/completer.go:70-75` (struct), `:93`, `:118`, `:188-195`, `:434-452`
- Modify: `internal/completer/completer_test.go`

**Interfaces:**
- Consumes: `dialect.SQLVariant`, `dialect.DriverVariant`, `dialect.DataBaseKeywordsForVariant`, `dialect.DataBaseFunctionsForVariant` (Task 3); `parser.ParseWithDriverVariant` (Task 4).
- Produces:
  - `completer.Completer` keeps `Driver dialect.DatabaseDriver` and gains `Variant dialect.SQLVariant`.
  - `func (c *Completer) driverVariant() dialect.DriverVariant` (unexported)
  - `func getLastWordWithVariant(text string, line, char int, dv dialect.DriverVariant) string` (unexported); `getLastWordWithDriver(text string, line, char int, driver dialect.DatabaseDriver) string` keeps its signature and delegates.

Note for the implementer: `candidates.go:168` and `:221` branch on `c.Driver == dialect.DatabaseDriverInterBase` for case-insensitive catalog matching. That is a *driver* property, not a dialect property — InterBase stores catalog names upper-cased in both dialects — so those two lines stay exactly as they are.

- [ ] **Step 1: Write the failing test**

Append to `internal/completer/completer_test.go`:

```go
func TestCompleteInterBaseKeywordsByVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant dialect.SQLVariant
		prefix  string
		want    []string
		absent  []string
	}{
		{
			name:    "dialect 1 has no TIME or TIMESTAMP type",
			variant: dialect.SQLVariantInterBase1,
			absent:  []string{"TIME", "TIMESTAMP"},
		},
		{
			// Absence alone would also hold if completion returned nothing at
			// all, so this case proves the pipeline runs under dialect 1.
			// TRIGGER is in the shared InterBase keyword list, not the
			// dialect 3 delta, so it must be offered under both variants.
			name:    "dialect 1 still offers its own keywords",
			variant: dialect.SQLVariantInterBase1,
			prefix:  "TRI",
			want:    []string{"TRIGGER"},
		},
		{
			name:    "dialect 3 offers TIME and TIMESTAMP",
			variant: dialect.SQLVariantInterBase3,
			want:    []string{"TIME", "TIMESTAMP"},
		},
		{
			name:    "dialect 3 keeps the shared keywords too",
			variant: dialect.SQLVariantInterBase3,
			prefix:  "TRI",
			want:    []string{"TRIGGER"},
		},
		{
			name:    "the default variant is dialect 3",
			variant: dialect.SQLVariantDefault,
			want:    []string{"TIME", "TIMESTAMP"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseCompletionCache(t))
			c.Driver = dialect.DatabaseDriverInterBase
			c.Variant = tt.variant

			text := tt.prefix
			if text == "" {
				text = "TIM"
			}
			got, err := c.Complete(text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: len(text)},
				},
			}, false)
			if err != nil {
				t.Fatal(err)
			}

			labels := completionLabels(got)
			for _, want := range tt.want {
				if !labels[want] {
					t.Errorf("missing keyword %q in %v", want, labels)
				}
			}
			for _, absent := range tt.absent {
				if labels[absent] {
					t.Errorf("unexpected keyword %q in %v", absent, labels)
				}
			}
		})
	}
}

func TestCompleteInterBaseDialect3DelimitedIdentifierPrefix(t *testing.T) {
	tests := []struct {
		name     string
		variant  dialect.SQLVariant
		wantCol  bool
	}{
		{name: "dialect 3 treats the double quote as an identifier start", variant: dialect.SQLVariantInterBase3, wantCol: true},
		{name: "dialect 1 treats it as a string", variant: dialect.SQLVariantInterBase1, wantCol: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseCompletionCache(t))
			c.Driver = dialect.DatabaseDriverInterBase
			c.Variant = tt.variant

			const text = `select rdb$ from rdb$database`
			got, err := c.Complete(text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: 11},
				},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			// Catalog column completion must keep working under both dialects:
			// the '$' word pattern is a driver property, not a dialect one.
			if !completionLabels(got)["RDB$RELATION_ID"] {
				t.Fatalf("missing InterBase catalog column in completions: %v", completionLabels(got))
			}
		})
	}
}

func TestGetLastWordWithVariantMatchesDriverForm(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	if got, want := getLastWordWithVariant("select rdb$", 1, 11, dv), "rdb$"; got != want {
		t.Errorf("getLastWordWithVariant() = %q, want %q", got, want)
	}
	if got, want := getLastWordWithVariant("select rdb$", 1, 11, dialect.DriverVariant{}), ""; got != want {
		t.Errorf("getLastWordWithVariant() with no driver = %q, want %q", got, want)
	}
	if got, want := getLastWordWithDriver("select rdb$", 1, 11, dialect.DatabaseDriverInterBase), "rdb$"; got != want {
		t.Errorf("getLastWordWithDriver() = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/completer/ -run 'Variant' -v`
Expected: FAIL to build — `c.Variant undefined (type *Completer has no field or method Variant)` and `undefined: getLastWordWithVariant`.

- [ ] **Step 3: Add the field and the variant-aware helpers**

In `internal/completer/completer.go`, replace the struct:

```go
type Completer struct {
	DBCache *database.DBCache
	Driver  dialect.DatabaseDriver
}
```

with:

```go
type Completer struct {
	DBCache *database.DBCache
	Driver  dialect.DatabaseDriver
	// Variant is the server-side SQL variant resolved for the active
	// connection. The empty variant selects the driver's default.
	Variant dialect.SQLVariant
}

func (c *Completer) driverVariant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: c.Driver, Variant: c.Variant}
}
```

Replace line 93:

```go
	parsed, err := parser.ParseWithDriverVariant(text, c.driverVariant())
```

Replace line 118:

```go
	lastWord := getLastWordWithVariant(text, params.Position.Line+1, params.Position.Character, c.driverVariant())
```

Replace lines 189 and 193:

```go
		drivers := dialect.DataBaseKeywordsForVariant(c.driverVariant())
```

```go
		drivers := dialect.DataBaseFunctionsForVariant(c.driverVariant())
```

Replace `getLastWordWithDriver` (line 438):

```go
// getLastWordWithDriver returns the word before the cursor using a driver's
// default variant. It is retained with its exact signature; see
// getLastWordWithVariant.
func getLastWordWithDriver(text string, line, char int, driver dialect.DatabaseDriver) string {
	return getLastWordWithVariant(text, line, char, dialect.DriverVariant{Driver: driver})
}

func getLastWordWithVariant(text string, line, char int, dv dialect.DriverVariant) string {
	t := getBeforeCursorText(text, line, char)
	s := getLine(t, line)

	wordPattern := "[\\w`]+$"
	if dv.Driver == dialect.DatabaseDriverInterBase {
		wordPattern = "[\\w$`]+$"
	}
	reg := regexp.MustCompile(wordPattern)
	ss := reg.FindAllString(s, -1)
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}
```

- [ ] **Step 4: Run the completer tests to verify they pass**

Run: `go test ./internal/completer/ -v`
Expected: PASS for every test, including the pre-existing `TestCompleteInterBaseDialect1DollarIdentifier`, `TestCompleteInterBaseJoinMatchesUppercaseCatalogNames` and `TestCompleteInterBaseJoinEscapesDollarInSnippet`, which set only `c.Driver` and therefore run on the default (Dialect 3) variant.

- [ ] **Step 5: Pin the existing Dialect 1 cases to their variant**

The four existing InterBase cases at `internal/completer/completer_test.go:187`, `:209`, `:266` and `:308` set `c.Driver` only. Add `c.Variant = dialect.SQLVariantInterBase1` immediately after each `c.Driver = dialect.DatabaseDriverInterBase`, so those tests keep testing what their names claim. Rename `TestCompleteInterBaseDialect1DollarIdentifier` and `TestCompleteInterBaseDialect1KeepsLowercaseUserNames` unchanged — they already name the dialect.

- [ ] **Step 6: Run the tests again**

Run: `go test ./internal/completer/ -v`
Expected: PASS.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/completer/completer.go internal/completer/completer_test.go
git commit -m "feat(completer): carry the resolved SQL variant through completion"
```

---

## Task 6: Formatter variant sibling and the `'c''d'` corruption guard

This is the task that pins the bug the `quotedStringEscapePreserver` interface exists to prevent. The formatter reprints tokens, so if a doubled quote is ever decoded, a format request silently rewrites the user's SQL.

**Files:**
- Modify: `internal/formatter/formatter.go:20-22`
- Modify: `internal/formatter/formatter_test.go`

**Interfaces:**
- Consumes: `dialect.DriverVariant`, `dialect.DialectForDriverVariant` (Task 3).
- Produces: `func FormatWithDriverVariant(text string, params lsp.DocumentFormattingParams, cfg *config.Config, dv dialect.DriverVariant) ([]lsp.TextEdit, error)`. `FormatWithDriver` keeps its signature and delegates.

- [ ] **Step 1: Write the failing test**

Append to `internal/formatter/formatter_test.go`:

```go
func TestFormatWithInterBaseVariantPreservesDoubledQuotes(t *testing.T) {
	// A format round trip must never rewrite 'c''d' as 'c'd'. Under Dialect 3
	// the double quote starts a delimited identifier, which is exactly the
	// condition the lexer previously used to decide escape preservation, so
	// this case is the regression guard for the optional lexer interface.
	tests := []struct {
		name    string
		variant dialect.SQLVariant
		want    string
	}{
		{
			name:    "dialect 1 reprints the double-quoted string verbatim",
			variant: dialect.SQLVariantInterBase1,
			want:    "SELECT\n\t\"a\"\"b\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "dialect 3 reprints the delimited identifier verbatim",
			variant: dialect.SQLVariantInterBase3,
			want:    "SELECT\n\t\"a\"\"b\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "the default variant behaves as dialect 3",
			variant: dialect.SQLVariantDefault,
			want:    "SELECT\n\t\"a\"\"b\",\n\t'c''d'\nFROM\n\trdb$database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const input = `select "a""b", 'c''d' from rdb$database`
			got, err := FormatWithDriverVariant(
				input,
				lsp.DocumentFormattingParams{},
				&config.Config{LowercaseKeywords: false},
				dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: tt.variant},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d text edits, want 1", len(got))
			}
			if got[0].NewText != tt.want {
				t.Fatalf("formatted query = %q, want %q", got[0].NewText, tt.want)
			}
			if !strings.Contains(got[0].NewText, `'c''d'`) {
				t.Fatalf("formatting corrupted the escaped string literal: %q", got[0].NewText)
			}
		})
	}
}

func TestFormatWithInterBaseDialect3DelimitedIdentifierWithSpace(t *testing.T) {
	got, err := FormatWithDriverVariant(
		`select "My Column" from t`,
		lsp.DocumentFormattingParams{},
		&config.Config{LowercaseKeywords: false},
		dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT\n\t\"My Column\"\nFROM\n\tt"; got[0].NewText != want {
		t.Fatalf("formatted query = %q, want %q", got[0].NewText, want)
	}
}
```

`strings` is already imported by `internal/formatter/formatter_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/formatter/ -run 'InterBaseVariant|InterBaseDialect3' -v`
Expected: FAIL to build — `undefined: FormatWithDriverVariant`.

- [ ] **Step 3: Add the sibling and delegate**

In `internal/formatter/formatter.go`, replace:

```go
func FormatWithDriver(text string, params lsp.DocumentFormattingParams, cfg *config.Config, driver dialect.DatabaseDriver) ([]lsp.TextEdit, error) {
	return formatWithDialect(text, params, cfg, dialect.DialectForDriver(driver))
}
```

with:

```go
// FormatWithDriver formats text using a driver's default variant. It is
// retained with its exact signature; see FormatWithDriverVariant.
func FormatWithDriver(text string, params lsp.DocumentFormattingParams, cfg *config.Config, driver dialect.DatabaseDriver) ([]lsp.TextEdit, error) {
	return FormatWithDriverVariant(text, params, cfg, dialect.DriverVariant{Driver: driver})
}

func FormatWithDriverVariant(text string, params lsp.DocumentFormattingParams, cfg *config.Config, dv dialect.DriverVariant) ([]lsp.TextEdit, error) {
	return formatWithDialect(text, params, cfg, dialect.DialectForDriverVariant(dv))
}
```

- [ ] **Step 4: Run the formatter tests to verify they pass**

Run: `go test ./internal/formatter/ -v`
Expected: PASS, including the pre-existing `TestFormatWithInterBaseDialect1` (which now exercises the default Dialect 3 variant and produces the same text, because both dialects reprint `"a""b"` and `'c''d'` verbatim).

- [ ] **Step 5: Commit**

```bash
git add internal/formatter/formatter.go internal/formatter/formatter_test.go
git commit -m "feat(formatter): add FormatWithDriverVariant and pin escaped-quote round trips"
```

---

## Task 7: Handler variant siblings and `parserDriverVariant()`

Every handler helper keeps its name and its `dialect.DatabaseDriver` parameter, gains a `…WithDriverVariant` sibling, and the server's own call sites move to the siblings. `parserDriver()` survives with its current signature.

**Files:**
- Modify: `internal/handler/handler.go:433-438`
- Modify: `internal/handler/hover.go:37,44-60`, `definition.go:34,37-46`, `rename.go:33,40-45`, `signature_help.go:32,39-48`, `execute_command.go:151,498-500`, `completion.go:29`, `format.go:28`
- Modify: `internal/handler/interbase_test.go`

**Interfaces:**
- Consumes: `dialect.DriverVariant` (Task 3), `parser.ParseWithDriverVariant` (Task 4), `completer.Completer.Variant` (Task 5), `formatter.FormatWithDriverVariant` (Task 6), `database.DBConnection` (unchanged in this task — `Variant` arrives in Task 9, so `parserDriverVariant` returns a zero `Variant` until then).
- Produces:
  - `func (s *Server) parserDriverVariant() dialect.DriverVariant`
  - `func (s *Server) parserDriver() dialect.DatabaseDriver` — retained, now `return s.parserDriverVariant().Driver`
  - `func hoverWithDriverVariant(text string, params lsp.HoverParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.Hover, error)`
  - `func definitionWithDriverVariant(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (lsp.Definition, error)`
  - `func renameWithDriverVariant(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, error)`
  - `func SignatureHelpWithDriverVariant(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.SignatureHelp, error)`
  - `func getStatementsWithDriverVariant(text string, dv dialect.DriverVariant) ([]*ast.Statement, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/handler/interbase_test.go`:

```go
func TestParserDriverVariant(t *testing.T) {
	s := NewServer()

	if got, want := s.parserDriverVariant(), (dialect.DriverVariant{}); got != want {
		t.Errorf("parserDriverVariant() without a connection = %#v, want %#v", got, want)
	}
	if got := s.parserDriver(); got != "" {
		t.Errorf("parserDriver() without a connection = %q, want empty", got)
	}

	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	if got, want := s.parserDriverVariant().Driver, dialect.DatabaseDriverInterBase; got != want {
		t.Errorf("parserDriverVariant().Driver = %q, want %q", got, want)
	}
	if got, want := s.parserDriver(), dialect.DatabaseDriverInterBase; got != want {
		t.Errorf("parserDriver() = %q, want %q", got, want)
	}
}

func TestInterBaseStatementParsingByVariant(t *testing.T) {
	const input = `select "a"";""b", 'c'';''d'; select rdb$database`

	tests := []struct {
		name    string
		variant dialect.SQLVariant
	}{
		{name: "dialect 1", variant: dialect.SQLVariantInterBase1},
		{name: "dialect 3", variant: dialect.SQLVariantInterBase3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statements, err := getStatementsWithDriverVariant(input, dialect.DriverVariant{
				Driver:  dialect.DatabaseDriverInterBase,
				Variant: tt.variant,
			})
			if err != nil {
				t.Fatal(err)
			}
			// A semicolon inside a quoted run must not split the statement,
			// under either dialect.
			if len(statements) != 2 {
				t.Fatalf("got %d statements, want 2", len(statements))
			}
			if got, want := statements[0].String(), `select "a"";""b", 'c'';''d';`; got != want {
				t.Errorf("first statement = %q, want %q", got, want)
			}
			if got, want := statements[1].String(), " select rdb$database"; got != want {
				t.Errorf("second statement = %q, want %q", got, want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/handler/ -run 'ParserDriverVariant|StatementParsingByVariant' -v`
Expected: FAIL to build — `s.parserDriverVariant undefined` and `undefined: getStatementsWithDriverVariant`.

- [ ] **Step 3: Add `parserDriverVariant` and make `parserDriver` delegate**

In `internal/handler/handler.go`, replace:

```go
func (s *Server) parserDriver() dialect.DatabaseDriver {
	if s.dbConn == nil {
		return ""
	}
	return s.dbConn.Driver
}
```

with:

```go
// parserDriver returns the active connection's driver. It is retained with its
// exact signature for callers that do not care about the SQL variant.
func (s *Server) parserDriver() dialect.DatabaseDriver {
	return s.parserDriverVariant().Driver
}

// parserDriverVariant returns the active connection's driver and its resolved
// server-side SQL variant. With no connection it returns the zero value, which
// selects each driver's default variant — for InterBase, SQL Dialect 3.
func (s *Server) parserDriverVariant() dialect.DriverVariant {
	if s.dbConn == nil {
		return dialect.DriverVariant{}
	}
	return s.dbConn.DriverVariant()
}
```

**Prerequisite: Task 9 must already be complete.** `DBConnection.DriverVariant()` arrives there, and Task 8's tests set `DBConnection.Variant`, which arrives there too. Task 9 depends on nothing from Tasks 7 or 8, so the execution order is **6 → 9 → 7 → 8 → 10**; the task numbers are kept as written so that cross-references elsewhere in this plan stay valid.

If you are executing Task 7 and `DBConnection.DriverVariant` does not exist, stop and do Task 9 first. Do not write a temporary body that reads `s.dbConn.Driver` directly: it compiles, it makes this task's tests pass, and it silently reports the default variant for every InterBase connection — which is the exact bug this plan exists to fix, reintroduced one layer up.

- [ ] **Step 4: Add the five helper siblings**

Each of the five is the same three-part edit: rename the existing function to `…WithDriverVariant` and change its last parameter to `dv dialect.DriverVariant`; change the one `parser.ParseWithDriver(text, driver)` line inside it to `parser.ParseWithDriverVariant(text, dv)`; and add a one-line retained function under the old name. Nothing else inside any body changes.

> **The `…WithDriverVariant` blocks below are not complete functions.** `// unchanged body, except:` and `// ...` are elisions: the existing body stays exactly as it is in the file, and the only line that changes is the `parser.ParseWithDriver` call. Do not paste these blocks over the real functions — you would delete five working implementations. Edit in place: change the signature line, change the one parse line, and add the wrapper above.

`internal/handler/hover.go` — rename `hoverWithDriver` (line 51) to `hoverWithDriverVariant`, change its signature and its parse call, then add the retained wrapper above it:

```go
func hoverWithDriver(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.Hover, error) {
	return hoverWithDriverVariant(text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func hoverWithDriverVariant(text string, params lsp.HoverParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.Hover, error) {
	// unchanged body, except:
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	// ...
}
```

`internal/handler/definition.go`:

```go
func definitionWithDriver(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (lsp.Definition, error) {
	return definitionWithDriverVariant(url, text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func definitionWithDriverVariant(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (lsp.Definition, error) {
	// unchanged body, except:
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	// ...
}
```

`internal/handler/rename.go`:

```go
func renameWithDriver(text string, params lsp.RenameParams, driver dialect.DatabaseDriver) (*lsp.WorkspaceEdit, error) {
	return renameWithDriverVariant(text, params, dialect.DriverVariant{Driver: driver})
}

func renameWithDriverVariant(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, error) {
	// unchanged body, except:
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	// ...
}
```

`internal/handler/signature_help.go` — this one is exported, so the sibling is exported too:

```go
func SignatureHelpWithDriver(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.SignatureHelp, error) {
	return SignatureHelpWithDriverVariant(text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func SignatureHelpWithDriverVariant(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.SignatureHelp, error) {
	// unchanged body, except:
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	// ...
}
```

`internal/handler/execute_command.go`:

```go
func getStatementsWithDriver(text string, driver dialect.DatabaseDriver) ([]*ast.Statement, error) {
	return getStatementsWithDriverVariant(text, dialect.DriverVariant{Driver: driver})
}

func getStatementsWithDriverVariant(text string, dv dialect.DriverVariant) ([]*ast.Statement, error) {
	// unchanged body, except:
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	// ...
}
```

Then move the five server call sites to the siblings:

| File and line | From | To |
| --- | --- | --- |
| `hover.go:37` | `hoverWithDriver(f.Text, params, s.worker.Cache(), s.parserDriver())` | `hoverWithDriverVariant(f.Text, params, s.worker.Cache(), s.parserDriverVariant())` |
| `definition.go:34` | `definitionWithDriver(params.TextDocument.URI, f.Text, params, s.worker.Cache(), s.parserDriver())` | `definitionWithDriverVariant(params.TextDocument.URI, f.Text, params, s.worker.Cache(), s.parserDriverVariant())` |
| `rename.go:33` | `renameWithDriver(f.Text, params, s.parserDriver())` | `renameWithDriverVariant(f.Text, params, s.parserDriverVariant())` |
| `signature_help.go:32` | `SignatureHelpWithDriver(f.Text, params, s.worker.Cache(), s.parserDriver())` | `SignatureHelpWithDriverVariant(f.Text, params, s.worker.Cache(), s.parserDriverVariant())` |
| `execute_command.go:151` | `getStatementsWithDriver(text, s.parserDriver())` | `getStatementsWithDriverVariant(text, s.parserDriverVariant())` |

The no-driver convenience wrappers (`hover.go:48`, `definition.go:38`, `rename.go:41`, `signature_help.go:40`) pass `""` and are unchanged — `dialect.DriverVariant{Driver: ""}` resolves to the generic dialect exactly as `DialectForDriver("")` does today.

- [ ] **Step 5: Move the completion and formatting call sites**

`internal/handler/completion.go:29` — replace:

```go
	c.Driver = s.parserDriver()
```

with:

```go
	dv := s.parserDriverVariant()
	c.Driver, c.Variant = dv.Driver, dv.Variant
```

`internal/handler/format.go:28` — replace:

```go
	textEdits, err := formatter.FormatWithDriver(f.Text, params, s.getConfig(), s.parserDriver())
```

with:

```go
	textEdits, err := formatter.FormatWithDriverVariant(f.Text, params, s.getConfig(), s.parserDriverVariant())
```

- [ ] **Step 6: Run the handler tests to verify they pass**

Run: `go test ./internal/handler/ -v`
Expected: PASS. The pre-existing `TestInterBaseDialect1StatementParsing` (`interbase_test.go:103-118`) still calls `getStatementsWithDriver` with the default variant and still expects two statements; the new `TestInterBaseStatementParsingByVariant` covers both dialects explicitly.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/handler/handler.go internal/handler/hover.go internal/handler/definition.go internal/handler/rename.go internal/handler/signature_help.go internal/handler/execute_command.go internal/handler/completion.go internal/handler/format.go internal/handler/interbase_test.go
git commit -m "feat(handler): add variant-aware siblings and parserDriverVariant"
```

---

## Task 8: End-to-end language-server variant behavior

With the whole propagation path in place, pin what the user actually sees: a Dialect 3 connection formats `"literal"` as an identifier and still emits `'c''d'`; a Dialect 1 connection does not; and with no connection at all, an InterBase document lexes as Dialect 3.

**Files:**
- Modify: `internal/handler/interbase_test.go:76-101,144-172`

**Interfaces:**
- Consumes: everything from Tasks 1-7; `database.DBConnection{Driver, Variant}` (Task 9 adds `Variant` — see the note in Step 1).

- [ ] **Step 1: Write the failing test**

This task's tests set `database.DBConnection{Driver: …, Variant: …}`, so **Task 9 must be complete first.** Reorder if necessary.

In `internal/handler/interbase_test.go`, change `configureInterBaseTestServer` (line 144) to take a variant:

```go
func configureInterBaseTestServer(t *testing.T, tx *TestContext, variant dialect.SQLVariant) {
	t.Helper()

	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "RDB$DATABASE", Name: "RDB$RELATION_ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Table: "RDB$DATABASE", Name: "RDB_OTHER"}, Type: "VARCHAR(20)"},
		{ColumnBase: database.ColumnBase{Table: "users", Name: "id"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"RDB$DATABASE", "users"}}, nil
		},
		MockDescribeDatabaseTable: func(context.Context) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	if err := tx.server.worker.ReCache(context.Background(), repo); err != nil {
		t.Fatal("worker.ReCache:", err)
	}
	tx.server.dbConn = &database.DBConnection{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: variant,
	}
}
```

Update its three existing callers (lines 18, 81, 125) to pass `dialect.SQLVariantInterBase1`, so those tests keep meaning what their names say. Then replace `TestInterBaseDialect1LanguageServerFormatting` (lines 76-101) with a table over both dialects:

```go
func TestInterBaseLanguageServerFormattingByVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant dialect.SQLVariant
		input   string
		want    string
	}{
		{
			name:    "dialect 1 treats double quotes as a string",
			variant: dialect.SQLVariantInterBase1,
			input:   `select "literal", 'c''d' from rdb$database`,
			want:    "SELECT\n\t\"literal\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "dialect 3 treats double quotes as a delimited identifier",
			variant: dialect.SQLVariantInterBase3,
			input:   `select "literal", 'c''d' from rdb$database`,
			want:    "SELECT\n\t\"literal\",\n\t'c''d'\nFROM\n\trdb$database",
		},
		{
			name:    "dialect 3 keeps a space inside a delimited identifier",
			variant: dialect.SQLVariantInterBase3,
			input:   `select "My Column" from rdb$database`,
			want:    "SELECT\n\t\"My Column\"\nFROM\n\trdb$database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			tx.textDocumentDidOpen(t, testFileURI, tt.input)
			params := lsp.DocumentFormattingParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			}

			var got []lsp.TextEdit
			if err := tx.conn.Call(tx.ctx, "textDocument/formatting", params, &got); err != nil {
				t.Fatal("conn.Call textDocument/formatting:", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d text edits, want 1", len(got))
			}
			if got[0].NewText != tt.want {
				t.Fatalf("formatted query = %q, want %q", got[0].NewText, tt.want)
			}
			if strings.Contains(tt.input, `'c''d'`) && !strings.Contains(got[0].NewText, `'c''d'`) {
				t.Fatalf("the language server corrupted an escaped string literal: %q", got[0].NewText)
			}
		})
	}
}

func TestInterBaseVariantReachesCompletionAndHover(t *testing.T) {
	// The variant on the connection must reach the completer, not just the
	// formatter: this is the end of the propagation chain that plan 1 builds.
	tests := []struct {
		name    string
		variant dialect.SQLVariant
	}{
		{name: "dialect 1", variant: dialect.SQLVariantInterBase1},
		{name: "dialect 3", variant: dialect.SQLVariantInterBase3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			const text = "select rdb$ from rdb$database"
			tx.textDocumentDidOpen(t, testFileURI, text)

			var completions []lsp.CompletionItem
			if err := tx.conn.Call(tx.ctx, "textDocument/completion", lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: 11},
				},
			}, &completions); err != nil {
				t.Fatal("conn.Call textDocument/completion:", err)
			}
			if labels := handlerCompletionLabels(completions); !labels["RDB$RELATION_ID"] {
				t.Errorf("missing catalog column completion under %s: %v", tt.variant, labels)
			}

			var hover lsp.Hover
			if err := tx.conn.Call(tx.ctx, "textDocument/hover", lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: 11},
				},
			}, &hover); err != nil {
				t.Fatal("conn.Call textDocument/hover:", err)
			}
			_ = hover
		})
	}

	// The dialect-dependent half: a Dialect 3 connection offers TIMESTAMP,
	// a Dialect 1 connection does not, because the type does not exist there.
	keywordTests := []struct {
		variant dialect.SQLVariant
		want    bool
	}{
		{variant: dialect.SQLVariantInterBase3, want: true},
		{variant: dialect.SQLVariantInterBase1, want: false},
	}
	for _, tt := range keywordTests {
		t.Run("TIMESTAMP offered for "+string(tt.variant), func(t *testing.T) {
			tx := newTestContext()
			tx.initServer(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()
			configureInterBaseTestServer(t, tx, tt.variant)

			const text = "TIM"
			tx.textDocumentDidOpen(t, testFileURI, text)

			var completions []lsp.CompletionItem
			if err := tx.conn.Call(tx.ctx, "textDocument/completion", lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: len(text)},
				},
			}, &completions); err != nil {
				t.Fatal("conn.Call textDocument/completion:", err)
			}
			if got := handlerCompletionLabels(completions)["TIMESTAMP"]; got != tt.want {
				t.Errorf("TIMESTAMP offered = %v, want %v under %s", got, tt.want, tt.variant)
			}
		})
	}
}

func TestParserVariantDefaultsToDialect3WithoutConnection(t *testing.T) {
	s := NewServer()
	if s.dbConn != nil {
		t.Fatal("a fresh server must have no connection")
	}
	dv := s.parserDriverVariant()
	if dv != (dialect.DriverVariant{}) {
		t.Fatalf("parserDriverVariant() = %#v, want the zero value", dv)
	}

	// The zero DriverVariant with the InterBase driver resolves to Dialect 3,
	// matching the interbase-go default. This is what fixes the bug offline.
	ib, ok := dialect.DialectForDriverVariant(dialect.DriverVariant{
		Driver: dialect.DatabaseDriverInterBase,
	}).(*dialect.InterBaseDialect)
	if !ok {
		t.Fatal("the InterBase driver must resolve to *InterBaseDialect")
	}
	if got, want := ib.SQLDialect, 3; got != want {
		t.Fatalf("offline InterBase SQLDialect = %d, want %d", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/handler/ -run 'FormattingByVariant|VariantReachesCompletion|VariantDefaultsToDialect3' -v`

Expected: FAIL to build until `configureInterBaseTestServer`'s three existing callers are updated to pass a variant. After that, two specific failures tell you an earlier task is incomplete rather than that this task needs code:

- `dialect 3 keeps a space inside a delimited identifier` fails → Task 1's `delimitedIdentifierScanner` is missing or not implemented by `InterBaseDialect`.
- `TIMESTAMP offered for interbase-dialect-3` fails → Task 5's `Completer.Variant` is not reaching `DataBaseKeywordsForVariant`, or Task 7's `completion.go` call site was not moved.

- [ ] **Step 3: Run the tests to verify they pass**

This task is deliberately implementation-free: Tasks 1-7 and 9 build everything it asserts, and its job is to pin the user-visible behavior end to end through the LSP. Any failure here is a real defect in an earlier task — fix it there, not by weakening an assertion.

Run: `go test ./internal/handler/ -v`
Expected: PASS, including the pre-existing `TestInterBaseDialect1LanguageServerCompletion` and `TestInterBaseDialect1LanguageServerHover`, which now pass `dialect.SQLVariantInterBase1` explicitly.

- [ ] **Step 4: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/interbase_test.go
git commit -m "test(handler): pin language-server formatting under both InterBase dialects"
```

---

## Task 9: `DBConnection` carries the variant, name and warnings

**Files:**
- Modify: `internal/database/driver.go:14-22` (types), `:74-80` (repository construction)
- Modify: `internal/database/interbase_common.go:15-18,91-98`
- Test: `internal/database/interbase_test.go`

**Interfaces:**
- Consumes: `dialect.SQLVariant`, `dialect.DriverVariant`, `dialect.InterBaseSQLVariant` (Task 3).
- Produces:
  - `DBConnection` gains `Variant dialect.SQLVariant`, `DatabaseName string`, `Warnings []string`
  - `func (db *DBConnection) DriverVariant() dialect.DriverVariant` — nil-safe
  - `type ConnFactory func(*DBConnection) DBRepository`
  - `func RegisterConnFactory(name dialect.DatabaseDriver, factory ConnFactory)`
  - `func CreateRepositoryFromConnection(driver dialect.DatabaseDriver, conn *DBConnection) (DBRepository, error)`
  - `func NewInterBaseDBRepositoryFromConnection(conn *DBConnection) DBRepository`
  - `InterBaseDBRepository` gains `SQLDialect int` and `DatabaseName string`

The two new repository fields are populated but not yet read: plan 2 (§4.3) reads `SQLDialect` for type rendering and plan 3 (§4.7) reads `DatabaseName` for `CurrentDatabase`/`Databases`. They are added here because they are what makes the `ConnFactory` seam observable and testable; without them the factory would be indistinguishable from the `*sql.DB` one.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -run 'DriverVariant|CreateRepositoryFromConnection|RepositoryDefaultsToDialect3' -v`
Expected: FAIL to build — `unknown field Variant in struct literal of type DBConnection`, `conn.DriverVariant undefined`, `undefined: CreateRepositoryFromConnection`, `unknown field SQLDialect`.

- [ ] **Step 3: Extend `DBConnection` and add the connection-aware factory**

In `internal/database/driver.go`, replace:

```go
var driverOpeners = make(map[dialect.DatabaseDriver]Opener)
var driverFactories = make(map[dialect.DatabaseDriver]Factory)

type Opener func(*DBConfig) (*DBConnection, error)
type Factory func(*sql.DB) DBRepository

type DBConnection struct {
	Conn    *sql.DB
	SSHConn *ssh.Client
	Tunnel  io.Closer
	Driver  dialect.DatabaseDriver
}
```

with:

```go
var driverOpeners = make(map[dialect.DatabaseDriver]Opener)
var driverFactories = make(map[dialect.DatabaseDriver]Factory)
var driverConnFactories = make(map[dialect.DatabaseDriver]ConnFactory)

type Opener func(*DBConfig) (*DBConnection, error)
type Factory func(*sql.DB) DBRepository

// ConnFactory builds a repository from the whole connection rather than from
// the *sql.DB alone, for drivers whose repository needs connection-level
// context such as a resolved SQL variant. It is optional: a driver that
// registers none keeps using Factory.
type ConnFactory func(*DBConnection) DBRepository

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
func (db *DBConnection) DriverVariant() dialect.DriverVariant {
	if db == nil {
		return dialect.DriverVariant{}
	}
	return dialect.DriverVariant{Driver: db.Driver, Variant: db.Variant}
}
```

Then append after `CreateRepository`:

```go
func RegisterConnFactory(name dialect.DatabaseDriver, factory ConnFactory) {
	if _, ok := driverConnFactories[name]; ok {
		panic(fmt.Sprintf("driver conn factory %s already registered", name))
	}
	driverConnFactories[name] = factory
}

// CreateRepositoryFromConnection builds a repository for a connection,
// preferring a registered ConnFactory and falling back to the *sql.DB factory
// so that drivers which register no ConnFactory are unaffected.
//
// The driver is passed in rather than read from conn.Driver because that field
// is not populated by every opener: openPostgreSQL (postgresql.go:55),
// openSQLite3 (sqlite3.go:24) and the "mock" opener (database_mock.go:549) all
// return a DBConnection with an empty Driver. The caller's configured driver is
// always populated, so keying the lookup off conn.Driver would return
// "driver not found" for PostgreSQL, SQLite3 and every handler test.
func CreateRepositoryFromConnection(driver dialect.DatabaseDriver, conn *DBConnection) (DBRepository, error) {
	if conn == nil {
		return nil, fmt.Errorf("connection is nil")
	}
	if factory, ok := driverConnFactories[driver]; ok {
		return factory(conn), nil
	}
	return CreateRepository(driver, conn.Conn)
}
```

- [ ] **Step 4: Register the InterBase conn factory**

In `internal/database/interbase_common.go`, replace the `init` block:

```go
func init() {
	RegisterOpen(dialect.DatabaseDriverInterBase, interBaseOpen)
	RegisterFactory(dialect.DatabaseDriverInterBase, NewInterBaseDBRepository)
	RegisterConnFactory(dialect.DatabaseDriverInterBase, NewInterBaseDBRepositoryFromConnection)
}
```

and replace the repository struct and constructor:

```go
type InterBaseDBRepository struct {
	Conn *sql.DB
	// SQLDialect is 1 or 3; zero is treated as 3, matching the driver default.
	SQLDialect int
	// DatabaseName is the attachment string; empty when unknown.
	DatabaseName string
}

var _ DBRepository = (*InterBaseDBRepository)(nil)

// NewInterBaseDBRepository builds a repository from a pooled *sql.DB alone.
// It has no connection context, so it leaves SQLDialect zero (dialect 3) and
// DatabaseName empty.
func NewInterBaseDBRepository(conn *sql.DB) DBRepository {
	return &InterBaseDBRepository{Conn: conn}
}

// NewInterBaseDBRepositoryFromConnection builds a repository that knows the
// SQL dialect resolved at connect and the attachment it was resolved for.
func NewInterBaseDBRepositoryFromConnection(conn *DBConnection) DBRepository {
	if conn == nil {
		return &InterBaseDBRepository{}
	}
	return &InterBaseDBRepository{
		Conn:         conn.Conn,
		SQLDialect:   conn.Variant.InterBaseSQLDialect(),
		DatabaseName: conn.DatabaseName,
	}
}
```

Note the asymmetry, and keep it: the `*sql.DB` factory leaves `SQLDialect` at `0` while the connection factory writes an explicit `1` or `3`. Both mean the same thing for a zero-or-three value, and leaving the zero uninterpreted keeps `NewInterBaseDBRepository` unchanged in behavior as the spec requires.

- [ ] **Step 5: Point the server at the new constructor**

In `internal/handler/handler.go`, replace the body of `newDBRepository` (line 386):

```go
func (s *Server) newDBRepository(ctx context.Context) (database.DBRepository, error) {
	if s.curDBCfg == nil || s.dbConn == nil {
		return nil, ErrNoConnection
	}
	repo, err := database.CreateRepositoryFromConnection(s.curDBCfg.Driver, s.dbConn)
	if err != nil {
		return nil, err
	}
	return repo, nil
}
```

The driver still comes from `s.curDBCfg.Driver`, exactly as the current body does — only the second argument is new. Do **not** "simplify" this to `s.dbConn.Driver`: the openers for PostgreSQL, SQLite3 and the test mock leave that field empty, and the whole `internal/handler` suite fails with `driver not found` if you do.

This task runs **before** Task 7 (execution order 6 → 9 → 7 → 8 → 10), so `parserDriverVariant` does not exist yet and there is nothing to replace here. If you find it already present with a body reading `s.dbConn.Driver` directly, someone ran Task 7 early against the instruction there — fix it to `return s.dbConn.DriverVariant()` now.

- [ ] **Step 6: Run the database and handler tests to verify they pass**

Run: `go test ./internal/database/ ./internal/handler/ -v`
Expected: PASS, including the pre-existing `TestInterBaseDriverIsRegistered` (`interbase_test.go:264`), which calls `CreateRepository(interbase, nil)` and must keep working.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/database/driver.go internal/database/interbase_common.go internal/database/interbase_test.go internal/handler/handler.go
git commit -m "feat(database): carry variant, database name and warnings on DBConnection"
```

---

## Task 10: `DBConfig.Dialect` and its validation

**Files:**
- Modify: `internal/database/config.go:20-33` (struct), `:36-43` (pre-switch validation), `:142-154` (InterBase case)
- Test: `internal/database/interbase_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `DBConfig.Dialect int` with tags `json:"dialect" yaml:"dialect"`, validated to `{0, 1, 3}` and rejected on non-InterBase drivers.

`DBConfig.InterBase *InterBaseConfig` from spec §4.6 is **plan 3's** field and is not added here.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -run 'ValidatesDialect|RejectedForNonInterBase' -v`
Expected: FAIL to build — `unknown field Dialect in struct literal of type DBConfig`.

- [ ] **Step 3: Add the field**

In `internal/database/config.go`, add to `DBConfig` after `Params`:

```go
	// Dialect selects the server-side SQL dialect. Only the interbase driver
	// supports it: 0 auto-detects from the database, 1 and 3 pin a dialect.
	Dialect        int                    `json:"dialect" yaml:"dialect"`
```

- [ ] **Step 4: Add the validation**

In `internal/database/config.go`, insert immediately after the `if c.Driver == "" { ... }` guard and before `switch c.Driver {`:

```go
	if c.Dialect != 0 && c.Driver != dialect.DatabaseDriverInterBase {
		return errors.New("invalid: connections[].dialect is only supported by the interbase driver")
	}
```

Then in the `case dialect.DatabaseDriverInterBase:` block, insert after the SSH check and before the `interBaseAttachment` call:

```go
		switch c.Dialect {
		case 0, 1, 3:
		default:
			return errors.New("invalid: connections[].dialect must be 0 (auto), 1, or 3")
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/database/ -v`
Expected: PASS, including the pre-existing config-validation table at `interbase_test.go:230-262`.

- [ ] **Step 6: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/config.go internal/database/interbase_test.go
git commit -m "feat(database): add and validate DBConfig.Dialect"
```

---

## Task 11: The dialect-resolution decision, untagged and unit-tested

The spec puts the resolution branch table inside the tagged `interBaseOpen`. Extracting the *decision* into a pure untagged function means every contributor can run its table with plain `go test ./...`, exactly as spec §4.3 justifies for the catalog code, and it means the branch table is verified before the driver dependency lands. The tagged file in Task 12 does the I/O and calls this.

**Files:**
- Create: `internal/database/interbase_dialect.go`
- Create: `internal/database/interbase_dialect_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type interBaseDialectDecision struct { Resolved int; Reattach bool; Warnings []string }`
  - `func resolveInterBaseDialect(alias string, requested int, reported int64, diagErr error) interBaseDialectDecision`
  
  `Resolved` is 1 or 3. `Reattach` is true only when the caller must close the first attachment and re-attach at `Resolved` — which happens for the auto-detected Dialect 1 case and nothing else. `reported` is ignored when `diagErr != nil`.

- [ ] **Step 1: Write the failing test**

Create `internal/database/interbase_dialect_test.go`:

```go
package database

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveInterBaseDialect(t *testing.T) {
	diagFailure := errors.New("info read failed")

	tests := []struct {
		name         string
		alias        string
		requested    int
		reported     int64
		diagErr      error
		wantResolved int
		wantReattach bool
		wantWarnings int
		wantMentions []string
	}{
		{
			name:         "auto-detect on a dialect 3 database attaches once",
			alias:        "centrale",
			requested:    0,
			reported:     3,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 0,
		},
		{
			name:         "auto-detect on a dialect 1 database costs one extra attach",
			alias:        "legacy",
			requested:    0,
			reported:     1,
			wantResolved: 1,
			wantReattach: true,
			wantWarnings: 0,
		},
		{
			name:         "auto-detect on an unexpected reported dialect falls back to 3 and warns",
			alias:        "odd",
			requested:    0,
			reported:     2,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"odd", "2", "3"},
		},
		{
			name:         "a pinned dialect that agrees is silent",
			alias:        "centrale",
			requested:    3,
			reported:     3,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 0,
		},
		{
			name:         "a pinned dialect 1 that disagrees connects and warns",
			alias:        "centrale",
			requested:    1,
			reported:     3,
			wantResolved: 1,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"centrale", "dialect 1", "dialect 3", "dialect: 0"},
		},
		{
			name:         "a pinned dialect 3 that disagrees connects and warns",
			alias:        "legacy",
			requested:    3,
			reported:     1,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"legacy", "dialect 3", "dialect 1"},
		},
		{
			name:         "a diagnostics failure with auto-detect falls back to 3 and warns",
			alias:        "offline",
			requested:    0,
			diagErr:      diagFailure,
			wantResolved: 3,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"offline", "diagnostics", "3"},
		},
		{
			name:         "a diagnostics failure with a pinned dialect keeps the pin and warns",
			alias:        "offline",
			requested:    1,
			diagErr:      diagFailure,
			wantResolved: 1,
			wantReattach: false,
			wantWarnings: 1,
			wantMentions: []string{"offline", "diagnostics", "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveInterBaseDialect(tt.alias, tt.requested, tt.reported, tt.diagErr)

			if got.Resolved != tt.wantResolved {
				t.Errorf("Resolved = %d, want %d", got.Resolved, tt.wantResolved)
			}
			if got.Reattach != tt.wantReattach {
				t.Errorf("Reattach = %v, want %v", got.Reattach, tt.wantReattach)
			}
			if len(got.Warnings) != tt.wantWarnings {
				t.Fatalf("got %d warnings, want %d: %v", len(got.Warnings), tt.wantWarnings, got.Warnings)
			}
			for _, mention := range tt.wantMentions {
				if !strings.Contains(got.Warnings[0], mention) {
					t.Errorf("warning %q does not mention %q", got.Warnings[0], mention)
				}
			}
			for _, warning := range got.Warnings {
				if strings.Contains(warning, "\n") {
					t.Errorf("warning must be a single line: %q", warning)
				}
				if !strings.HasPrefix(warning, "interbase: ") {
					t.Errorf("warning must be prefixed for the log: %q", warning)
				}
			}
		})
	}
}

func TestResolveInterBaseDialectReattachesOnlyForAutoDetectedDialect1(t *testing.T) {
	// The locked cost model: exactly one extra attach, and only here.
	for _, requested := range []int{0, 1, 3} {
		for _, reported := range []int64{1, 3} {
			got := resolveInterBaseDialect("a", requested, reported, nil)
			want := requested == 0 && reported == 1
			if got.Reattach != want {
				t.Errorf("requested=%d reported=%d: Reattach = %v, want %v", requested, reported, got.Reattach, want)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -run ResolveInterBaseDialect -v`
Expected: FAIL to build — `undefined: resolveInterBaseDialect`.

- [ ] **Step 3: Write the decision function**

Create `internal/database/interbase_dialect.go`:

```go
package database

import "fmt"

// interBaseDialectDecision is the outcome of resolving the SQL dialect for a
// connection: the dialect to use, whether the caller must re-attach at it, and
// any non-fatal diagnostics for the user.
type interBaseDialectDecision struct {
	// Resolved is 1 or 3.
	Resolved int
	// Reattach is true only when the first attachment used the driver default
	// and the database turned out to be Dialect 1, so it must be replaced.
	Reattach bool
	// Warnings are single-line, credential-free messages for the user.
	Warnings []string
}

// resolveInterBaseDialect decides which SQL dialect a connection uses.
//
// requested is DBConfig.Dialect: 0 auto-detects, 1 and 3 pin a dialect.
// reported is interbase.DatabaseDiagnostics.SQLDialect, the value the server
// answers for this attachment; it is ignored when diagErr is non-nil.
//
// Auto-detect costs exactly one extra attach, and only for a Dialect 1
// database: the first attachment already used the driver default of 3, so a
// reported 3 needs no second attach.
//
// A pinned dialect that disagrees with the database is a warning, not a
// failure. Attaching a client at a dialect different from the database's own is
// a supported InterBase configuration — it is the normal way to read a Dialect
// 1 database from Dialect 3 tooling during a migration — the server enforces
// its own rules regardless, and refusing to attach would take away a working
// setup the user asked for explicitly.
//
// A diagnostics failure is never fatal either: metadata introspection must not
// block editing.
func resolveInterBaseDialect(alias string, requested int, reported int64, diagErr error) interBaseDialectDecision {
	switch {
	case diagErr != nil:
		resolved := requested
		if resolved == 0 {
			resolved = 3
		}
		return interBaseDialectDecision{
			Resolved: resolved,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q could not read database diagnostics (%v); using SQL dialect %d.",
				alias, diagErr, resolved,
			)},
		}

	case requested == 0 && reported == 1:
		return interBaseDialectDecision{Resolved: 1, Reattach: true}

	case requested == 0 && reported == 3:
		return interBaseDialectDecision{Resolved: 3}

	case requested == 0:
		return interBaseDialectDecision{
			Resolved: 3,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q reports an unexpected SQL dialect %d; using SQL dialect 3.",
				alias, reported,
			)},
		}

	case int64(requested) != reported:
		return interBaseDialectDecision{
			Resolved: requested,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q is configured for SQL dialect %d but the database reports SQL dialect %d; "+
					"sqls will lex and render types as dialect %d. Remove `dialect` or set `dialect: 0` to follow the database.",
				alias, requested, reported, requested,
			)},
		}

	default:
		return interBaseDialectDecision{Resolved: requested}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/database/ -run ResolveInterBaseDialect -v`
Expected: PASS, ten subtests plus the reattach-cost test.

- [ ] **Step 5: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/interbase_dialect.go internal/database/interbase_dialect_test.go
git commit -m "feat(database): add the InterBase dialect resolution decision table"
```

---

## Task 12: Wire the connect flow (tagged)

**This task does not compile until `interbase.Diagnostics` exists in the driver** — see the Global Constraints dependency note. Everything it needs on the sqls side is already built and tested by Tasks 9-11. It lives entirely behind `//go:build interbase && cgo && linux && amd64`, so `go test ./...` stays green whether or not this task is done.

**Files:**
- Modify: `internal/database/interbase_native.go`
- Modify: `internal/database/interbase_live_test.go`

**Interfaces:**
- Consumes: `resolveInterBaseDialect` (Task 11), `DBConnection{Variant, DatabaseName, Warnings}` (Task 9), `DBConfig.Dialect` (Task 10), `dialect.InterBaseSQLVariant` (Task 3), `interbase.Diagnostics(ctx, *sql.Conn) (interbase.DatabaseDiagnostics, error)` (driver, unimplemented).
- Produces: `interBaseOpen` returns a `*DBConnection` whose `Variant`, `DatabaseName` and `Warnings` are populated. Its signature is unchanged.

- [ ] **Step 1: Write the failing live tests**

Append to `internal/database/interbase_live_test.go`:

```go
func interBaseLiveConfig(t *testing.T, sqlDialect int) *DBConfig {
	t.Helper()
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}
	return &DBConfig{
		Alias:          "live",
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
		Dialect:        sqlDialect,
	}
}

// interBaseLiveReportedDialect reads the dialect the server reports for a
// connection, so the auto-detect test can self-check without a fixture.
func interBaseLiveReportedDialect(t *testing.T, connection *DBConnection) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := connection.Conn.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	diagnostics, err := interbase.Diagnostics(ctx, conn)
	if err != nil {
		t.Fatalf("interbase.Diagnostics() error = %v", err)
	}
	return diagnostics.SQLDialect
}

// TestInterBaseLiveDialectAutoDetect must be run against both a Dialect 1 and a
// Dialect 3 database to be meaningful. Set INTERBASE_EXPECT_DIALECT to pin the
// expectation; otherwise the test self-checks against the server's own answer.
func TestInterBaseLiveDialectAutoDetect(t *testing.T) {
	cfg := interBaseLiveConfig(t, 0)

	connection, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	reported := interBaseLiveReportedDialect(t, connection)
	want := int(reported)
	if expected := os.Getenv("INTERBASE_EXPECT_DIALECT"); expected != "" {
		parsed, err := strconv.Atoi(expected)
		if err != nil {
			t.Fatalf("INTERBASE_EXPECT_DIALECT = %q is not a number", expected)
		}
		if parsed != want {
			t.Fatalf("INTERBASE_EXPECT_DIALECT = %d but the server reports %d", parsed, want)
		}
	}

	if got := connection.Variant.InterBaseSQLDialect(); got != want {
		t.Fatalf("auto-detected variant = %q (dialect %d), want dialect %d", connection.Variant, got, want)
	}
	if len(connection.Warnings) != 0 {
		t.Errorf("auto-detect against a %d-dialect database warned: %v", want, connection.Warnings)
	}
	if connection.DatabaseName != cfg.DataSourceName {
		t.Errorf("DatabaseName = %q, want %q", connection.DatabaseName, cfg.DataSourceName)
	}
	t.Logf("server reports SQL dialect %d; resolved variant %q", reported, connection.Variant)
}

func TestInterBaseLiveExplicitDialectMismatchWarnsAndConnects(t *testing.T) {
	probe, err := Open(interBaseLiveConfig(t, 0))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	reported := interBaseLiveReportedDialect(t, probe)
	_ = probe.Close()

	opposite := 1
	if reported == 1 {
		opposite = 3
	}

	cfg := interBaseLiveConfig(t, opposite)
	if cfg.Alias == "" {
		t.Fatal("interBaseLiveConfig must set a non-empty Alias; the alias assertion below is vacuous without one")
	}
	connection, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() with a mismatched dialect must connect, got error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if got := connection.Variant.InterBaseSQLDialect(); got != opposite {
		t.Errorf("resolved dialect = %d, want the configured %d", got, opposite)
	}
	if len(connection.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(connection.Warnings), connection.Warnings)
	}
	warning := connection.Warnings[0]
	for _, mention := range []string{
		fmt.Sprintf("dialect %d", opposite),
		fmt.Sprintf("dialect %d", reported),
	} {
		if !strings.Contains(warning, mention) {
			t.Errorf("warning %q does not mention %q", warning, mention)
		}
	}
	// A user with several connections needs to know which one warned.
	if !strings.Contains(warning, cfg.Alias) {
		t.Errorf("warning %q does not name the connection alias %q", warning, cfg.Alias)
	}
}
```

Add `fmt`, `strconv` and `interbase "interbase-go"` to the imports of `internal/database/interbase_live_test.go`.

- [ ] **Step 2: Run the live tests to verify they fail**

Run: `CGO_ENABLED=1 go build -tags interbase ./...`
Expected: FAIL to build — `connection.Variant undefined` is already fixed by Task 9, so the actual failure is in `interbase_native.go` not populating `Variant`/`DatabaseName`/`Warnings`, and (until the driver work lands) `undefined: interbase.Diagnostics`.

With a server available:

```shell
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLiveDialect|^TestInterBaseLiveExplicit' -count=1 -v -timeout=50s
```

Expected: FAIL — `auto-detected variant = "" (dialect 3), want dialect 1` on a Dialect 1 database, and `got 0 warnings, want 1` on the mismatch test.

- [ ] **Step 3: Write the connect flow**

Replace the whole body of `interBaseOpen` in `internal/database/interbase_native.go`:

```go
//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sqls-server/sqls/dialect"
	interbase "interbase-go"
)

const interBasePingTimeout = 10 * time.Second

// interBaseAttach opens and pings a pooled connection at one SQL dialect.
// A zero sqlDialect uses the driver default, which normalizeDialect maps to 3.
func interBaseAttach(cfg *DBConfig, attachment, charset string, sqlDialect int) (*sql.DB, error) {
	connector, err := interbase.NewConnector(interbase.Config{
		Database: attachment,
		User:     cfg.User,
		Password: cfg.Passwd,
		Charset:  charset,
		Dialect:  sqlDialect,
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: create connector: %w", err)
	}

	conn := sql.OpenDB(connector)
	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)

	ctx, cancel := context.WithTimeout(context.Background(), interBasePingTimeout)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("interbase: ping failed: %w", err)
	}
	return conn, nil
}

// interBaseDiagnostics reads the database's own answers over a pooled
// connection. Its error is never fatal: metadata introspection must not block
// editing, so the caller degrades to a default dialect and warns.
func interBaseDiagnostics(conn *sql.DB) (interbase.DatabaseDiagnostics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), interBasePingTimeout)
	defer cancel()

	pooled, err := conn.Conn(ctx)
	if err != nil {
		return interbase.DatabaseDiagnostics{}, err
	}
	defer func() { _ = pooled.Close() }()

	return interbase.Diagnostics(ctx, pooled)
}

func interBaseOpen(cfg *DBConfig) (*DBConnection, error) {
	if cfg == nil {
		return nil, errors.New("interbase: connection config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	attachment, err := interBaseAttachment(cfg)
	if err != nil {
		return nil, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return nil, err
	}

	alias := cfg.Alias
	if alias == "" {
		alias = attachment
	}

	// The first attach uses the requested dialect; a requested zero means the
	// driver default, which is 3.
	conn, err := interBaseAttach(cfg, attachment, charset, cfg.Dialect)
	if err != nil {
		return nil, err
	}

	diagnostics, diagErr := interBaseDiagnostics(conn)
	decision := resolveInterBaseDialect(alias, cfg.Dialect, diagnostics.SQLDialect, diagErr)

	if decision.Reattach {
		// The single extra attach, paid only by a Dialect 1 database.
		_ = conn.Close()
		conn, err = interBaseAttach(cfg, attachment, charset, decision.Resolved)
		if err != nil {
			return nil, err
		}
	}

	return &DBConnection{
		Conn:         conn,
		Driver:       dialect.DatabaseDriverInterBase,
		Variant:      dialect.InterBaseSQLVariant(decision.Resolved),
		DatabaseName: attachment,
		Warnings:     decision.Warnings,
	}, nil
}
```

- [ ] **Step 4: Build and run the tagged suite**

Run: `CGO_ENABLED=1 go build -tags interbase ./...`
Expected: success once `interbase.Diagnostics` exists.

Run: `CGO_ENABLED=1 go test -tags interbase ./...`
Expected: every package `ok`; the live tests skip without `INTERBASE_*`.

With a server:

```shell
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```

Expected: PASS. Run it against both a Dialect 1 and a Dialect 3 database; `TestInterBaseLiveDialectAutoDetect` is only meaningful across both.

- [ ] **Step 5: Run the untagged suite**

Run: `go test ./...`
Expected: every package `ok`, unchanged — this task touched only tagged files.

- [ ] **Step 6: Commit**

```bash
git add internal/database/interbase_native.go internal/database/interbase_live_test.go
git commit -m "feat(database): resolve the InterBase SQL dialect at connect"
```

---

## Task 13: Surface connect-time warnings to the user

**Files:**
- Modify: `internal/handler/handler.go:139-190` (`handleInitialize`), `:309-338` (`handleWorkspaceDidChangeConfiguration`), `:341-359` (`reconnectionDB`)
- Test: `internal/handler/interbase_test.go`

**Interfaces:**
- Consumes: `DBConnection.Warnings` (Task 9), `lsp.Messenger.ShowWarning` (`internal/lsp/client.go:45`, existing).
- Produces:
  - `func (s *Server) reconnectionDB(ctx context.Context) error` — unchanged signature; now logs each warning with `log.Println`.
  - `func (s *Server) showConnectionWarnings(ctx context.Context, messenger lsp.MessageDisplayer)` — sends each warning through `ShowWarning`; a nil messenger or an empty warning list is a no-op.

`execute_command.go:394` (`switchDatabase`) and `:462` (`switchConnections`) call `reconnectionDB` without a `*jsonrpc2.Conn`. They are deliberately left alone: their warnings are logged by `reconnectionDB` and not shown as a toast, because those paths have no messenger and the spec introduces no new LSP plumbing.

- [ ] **Step 1: Write the failing test**

Append to `internal/handler/interbase_test.go`:

```go
type recordingMessenger struct {
	warnings []string
	infos    []string
	errs     []string
}

func (m *recordingMessenger) ShowLog(context.Context, string) error { return nil }

func (m *recordingMessenger) ShowInfo(_ context.Context, message string) error {
	m.infos = append(m.infos, message)
	return nil
}

func (m *recordingMessenger) ShowWarning(_ context.Context, message string) error {
	m.warnings = append(m.warnings, message)
	return nil
}

func (m *recordingMessenger) ShowError(_ context.Context, message string) error {
	m.errs = append(m.errs, message)
	return nil
}

func TestShowConnectionWarnings(t *testing.T) {
	const warning = `interbase: connection "centrale" is configured for SQL dialect 1 but the database reports SQL dialect 3; sqls will lex and render types as dialect 1. Remove ` + "`dialect`" + ` or set ` + "`dialect: 0`" + ` to follow the database.`

	t.Run("warnings reach the messenger", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{
			Driver:   dialect.DatabaseDriverInterBase,
			Variant:  dialect.SQLVariantInterBase1,
			Warnings: []string{warning},
		}
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)

		if len(messenger.warnings) != 1 {
			t.Fatalf("got %d warnings, want 1: %v", len(messenger.warnings), messenger.warnings)
		}
		if messenger.warnings[0] != warning {
			t.Errorf("warning = %q, want %q", messenger.warnings[0], warning)
		}
		if len(messenger.errs) != 0 || len(messenger.infos) != 0 {
			t.Error("a connect warning must not be shown as an error or an info")
		}
	})

	t.Run("no connection is a no-op", func(t *testing.T) {
		s := NewServer()
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)
		if len(messenger.warnings) != 0 {
			t.Errorf("got %d warnings without a connection, want 0", len(messenger.warnings))
		}
	})

	t.Run("no warnings is a no-op", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
		messenger := &recordingMessenger{}
		s.showConnectionWarnings(context.Background(), messenger)
		if len(messenger.warnings) != 0 {
			t.Errorf("got %d warnings, want 0", len(messenger.warnings))
		}
	})

	t.Run("a nil messenger does not panic", func(t *testing.T) {
		s := NewServer()
		s.dbConn = &database.DBConnection{Warnings: []string{warning}}
		s.showConnectionWarnings(context.Background(), nil)
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/handler/ -run TestShowConnectionWarnings -v`
Expected: FAIL to build — `s.showConnectionWarnings undefined`.

- [ ] **Step 3: Add the warning surfaces**

In `internal/handler/handler.go`, add after `reconnectionDB`:

```go
// showConnectionWarnings sends any non-fatal connect-time diagnostics to the
// client. It is a no-op without a connection, without warnings, or without a
// messenger — the two paths that reach reconnectionDB from a command have no
// *jsonrpc2.Conn, and there those warnings are logged only.
func (s *Server) showConnectionWarnings(ctx context.Context, messenger lsp.MessageDisplayer) {
	if s.dbConn == nil || messenger == nil {
		return
	}
	for _, warning := range s.dbConn.Warnings {
		if err := messenger.ShowWarning(ctx, warning); err != nil {
			log.Println("send warning", err.Error())
		}
	}
}
```

In `reconnectionDB`, log the warnings after the connection is stored. Replace:

```go
	s.dbConn = dbConn
	dbRepo, err := s.newDBRepository(ctx)
```

with:

```go
	s.dbConn = dbConn
	for _, warning := range dbConn.Warnings {
		log.Println(warning)
	}
	dbRepo, err := s.newDBRepository(ctx)
```

- [ ] **Step 4: Call it from the two handlers that hold a connection**

In `handleInitialize`, the existing block at lines 175-190 ends with `}` then `return result, nil`. Insert the call so warnings are shown after a *successful* reconnection too:

```go
	messenger := lsp.NewMessenger(conn)
	if err := s.reconnectionDB(ctx); err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}
	s.showConnectionWarnings(ctx, messenger)
	return result, nil
```

Apply the identical insertion in `handleWorkspaceDidChangeConfiguration`, immediately before its final `return nil, nil` (line 337).

- [ ] **Step 5: Run the handler tests to verify they pass**

Run: `go test ./internal/handler/ -v`
Expected: PASS, including all four `TestShowConnectionWarnings` subtests.

- [ ] **Step 6: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/handler.go internal/handler/interbase_test.go
git commit -m "feat(handler): surface InterBase connect warnings through ShowWarning"
```

---

## Task 14: README item 1 — dialects

Only item 1 of the spec's six-item README rewrite belongs to this plan. Items 2, 3 and 5 (connection keys, TLS, single attachment) are plan 3; item 4 (metadata depth) is plan 2. The stale sentences that plan 1 directly contradicts are removed here.

**Files:**
- Modify: `README.md:290`, `:313-325`

**Interfaces:** none.

- [ ] **Step 1: Retitle the section**

Replace line 290:

```markdown
#### InterBase (SQL Dialect 1)
```

with:

```markdown
#### InterBase
```

- [ ] **Step 2: Replace the dialect paragraphs**

Replace lines 315-325, which currently read:

```markdown
The driver always uses client SQL Dialect 1; no dialect parameter is necessary.
Both single and double quotes delimit strings, doubled quotes escape a quote,
unquoted identifiers may contain `$`, and positional parameters use `?`.
Parsing and formatting use these rules when the selected connection is
InterBase. Parameter binding is a driver capability; the sqls execute command
does not prompt for parameter values.

Completion and hover use user table/view, column, primary-key, and foreign-key
metadata. InterBase has no schema namespace or database enumeration through this
adapter, so switching databases is not supported; configure separate connections
instead. Dialect 1 `DATE` includes both date and time. Dialect 3 is not supported.
```

with the text below.

> **Fence warning.** The replacement contains a ```` ```yaml ```` example, so it is a
> fenced block inside a fenced block. Most renderers — and a naive copy — will
> stop at the inner closing fence and drop everything after the YAML. What goes
> into `README.md` is everything between the outer ```` ```markdown ```` and the
> **final** ```` ``` ````, including the YAML example and both paragraphs that
> follow it. Step 3's greps confirm the tail arrived.

```markdown
Both SQL Dialect 1 and SQL Dialect 3 are supported. The optional `dialect` key
accepts `0` (the default, auto-detect from the database), `1` or `3`:

```yaml
connections:
  - alias: interbase_example
    driver: interbase
    dataSourceName: "db.example.test/3050:/srv/interbase/example.ib"
    user: sqls_reader
    passwd: "your-password"
    dialect: 0            # 0 auto-detect (default), 1, or 3
    params:
      charset: UTF8
```

Auto-detection asks the server which dialect the database uses and costs one
extra attachment only for a Dialect 1 database; a Dialect 3 database is detected
on the first attachment. Pinning `dialect:` is only needed to override
auto-detection. A pinned dialect that disagrees with the database is a supported
InterBase configuration — it is how Dialect 3 tooling reads a Dialect 1 database
during a migration — so sqls connects and shows a warning rather than refusing.
If the server cannot answer, sqls falls back to Dialect 3 and warns.

Under Dialect 1, double quotes delimit strings and `DATE` carries a time
component. Under Dialect 3, double quotes delimit identifiers, so `"My Column"`
is a column name, and `TIMESTAMP` is distinct from `DATE`. In both dialects,
unquoted identifiers may contain `$`, positional parameters use `?`, and doubled
quotes are preserved verbatim by the formatter, so formatting never rewrites
`'c''d'`. Parsing, completion and formatting use the resolved dialect's rules
when the selected connection is InterBase; with no connection open, InterBase
documents are parsed as Dialect 3, matching the driver default. Parameter
binding is a driver capability; the sqls execute command does not prompt for
parameter values.

Completion and hover use user table/view, column, primary-key, and foreign-key
metadata. InterBase has no schema namespace or database enumeration through this
adapter, so switching databases is not supported; configure separate connections
instead.
```

- [ ] **Step 3: Verify no stale claim survives**

Run: `grep -n 'Dialect 3 is not supported\|always uses client SQL Dialect 1\|SQL Dialect 1)' README.md`
Expected: no output.

Run: `grep -n 'dialect' README.md | head -20`
Expected: the new paragraphs, and no claim that a dialect parameter is unnecessary.

Run: `grep -c 'switching databases is not supported' README.md`
Expected: `1`. This is the last line of the replacement, after the nested YAML
fence — a zero here means the paste stopped at the inner fence and the tail of
the section is missing.

The TLS sentence at what was line 328 ("the driver exposes no TLS configuration API") stays for now: it is still true until plan 3 lands TLS support, and rewriting it is plan 3's README item 3.

- [ ] **Step 4: Run the whole suite one final time**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document InterBase dialect resolution"
```

---

## Verification checklist

Before declaring the plan complete, confirm each of these by running the command and reading the output — not by assuming:

- [ ] `go test ./...` — every package `ok`.
- [ ] `CGO_ENABLED=1 go build -tags interbase ./...` — builds (requires the driver's `Diagnostics`).
- [ ] `go test ./token/ -run TestTokenizeQuotedStringEscapePreservation -v` — only the generic dialect decodes `''`.
- [ ] `go test ./parser/ -run TestParseInterBaseDoubleQuotedText -v` — both dialects preserve `'c''d'`.
- [ ] `go test ./internal/formatter/ -run TestFormatWithInterBaseVariantPreservesDoubledQuotes -v` — all three variants.
- [ ] `go test ./internal/handler/ -run TestInterBaseLanguageServerFormattingByVariant -v` — end to end.
- [ ] `go test ./internal/handler/ -run TestInterBaseVariantReachesCompletionAndHover -v` — the variant reaches completion.
- [ ] `grep -rn 'func DialectForDriver(driver DatabaseDriver) Dialect' dialect/` — signature unchanged.
- [ ] `grep -rn 'func ParseWithDriver(text string, driver dialect.DatabaseDriver)' parser/` — signature unchanged.
- [ ] `grep -rn 'func (s \*Server) parserDriver() dialect.DatabaseDriver' internal/handler/` — signature unchanged.
- [ ] `grep -rn 'Driver  dialect.DatabaseDriver' internal/completer/completer.go` — `Completer.Driver` retained.
- [ ] With a live Dialect 1 server *and* a live Dialect 3 server: `timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s`.
