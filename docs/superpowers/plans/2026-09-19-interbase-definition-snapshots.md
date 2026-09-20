# Go-to-Definition Snapshots Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `textDocument/definition` jump to the source of an InterBase procedure, view or trigger by materialising the catalog's own text as a read-only `file://` snapshot the editor can actually open.

**Architecture:** A new filesystem subsystem, `sourceSnapshotStore`, owns an **injected** root directory, one directory per connection per process named `<hash>-<pid>`, `0700` directories and `0600` files, a 24-hour mtime prune that runs once before the first write, and removal of everything it created on shutdown. A thin LSP layer resolves the identifier under the cursor against the catalog cache, asks `DDLRepository.ObjectDDL` for executable DDL, falls back to the descriptor's verbatim catalog source when the catalog cannot reproduce DDL, writes the file and returns a zero-width `lsp.Location` at the object's name. The existing in-document alias resolution runs first and always wins, so no driver loses behaviour it has today.

**Tech Stack:** Go 1.25.7, `os`/`path/filepath`/`crypto/sha256`/`net/url`/`sync`/`regexp` from the standard library, `github.com/sourcegraph/jsonrpc2@v0.2.1`. Nothing in this plan imports `interbase-go`, and this plan adds no build-tagged file.

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-editor-features-design.md` — this plan implements **Plan 4 of 4** from that spec's "Plan decomposition" section: feature 5 (§5) in full, plus the go-to-definition paragraphs of the README. Features 1–4 and all of §6 belong to other plans and must **not** be built here.

**Builds on:**

- `docs/superpowers/plans/2026-09-19-server-concurrency-cancellation.md` (Plan 1), which must be complete before this plan starts. This plan consumes Plan 1's `stateMu`, its `fileText` accessor, its restructured `Server.Stop`, its nil-guarded `DBConnection.Close`, its `failingCloser` test type in `internal/handler/dispatch_test.go`, and `make test-race`.
- Sub-project 2's catalog work, for Tasks 4–6 only. Tasks 1–3 and 7 depend on nothing outside this repository and can be executed the moment Plan 1 lands. Task 4 opens with a precondition step that verifies the contract symbols exist and stops if they do not.

**Runs alongside:** Plan 3 (`features 1–4`) is being written and executed in parallel and also touches `internal/handler/hover.go`, `internal/completer/` and `reconnectionDB`. Two shared names are flagged in File Structure rather than claimed here.

## Global Constraints

- **Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu` is never held across any I/O. Inherited verbatim from Plan 1 and binding on every task here. **Writing a file is I/O.** The snapshot store copies the connection identity out from under `stateMu` into a `snapshotContext` value and does every filesystem call with that lock released.
- The snapshot store's own mutex (`sourceSnapshotStore.mu`) *is* held across filesystem calls. That is allowed and deliberate: the invariant above names `stateMu`, and the store's mutex is a leaf lock that nothing else in the server ever takes.
- `textDocument/definition` stays on the **inline** dispatch path (Plan 1: "Hover, completion, signature help, definition, formatting and rename stay inline. Only `workspace/executeCommand` is dispatched asynchronously"). It takes `stateMu` and never `connMu`.
- Any new `Server` field must be added to the concurrency audit table in `doc/develop.md` and classified (Plan 1, Global Constraints). This plan adds two.
- **URI scheme is ordinary `file://`.** The `sqls-interbase://` virtual-document design was considered and **rejected**: "Returning a URI the client cannot open is worse than returning nothing."
- **The store root is an injected field**, resolved once at construction. Never call `os.UserCacheDir()` at a use site: it returns `(string, error)` so it cannot be nested in `filepath.Join` at all, and `t.Setenv("XDG_CACHE_HOME", …)` only steers it on Linux and BSD — on macOS a prune test driven by the environment variable would delete from a real user cache directory, and ubuntu-only CI would never reveal it. **Every test constructs the store with `root: t.TempDir()` directly.**
- Directories are created `0o700`, files `0o600`. The attachment string "is hashed, never written: it can contain a host and a path, and must not be left on disk in clear form."
- **No `CREATE` header is ever synthesized.** On `ErrUnsupportedDDL` the body is the **verbatim** `ProcedureDesc.Source` / `ViewDesc.ViewSource` / `TriggerDesc.Source`, preceded by a comment naming the blocking feature. "The user never sees a fabricated declaration."
- On `errors.Is(err, database.ErrObjectNotFound)`, **no file is written at all** and definition returns `nil, nil`. Same for `ErrUnsupportedDDL` when the descriptor's source field is also invalid, and for any other error.
- "The existing alias/subquery resolution runs first and wins." Every case in `definitionTestCases` (`internal/handler/definition_test.go`) must stay green, unchanged, for every driver.
- An empty `TriggerDesc.Event` means "the catalog did not tell us" — omit, never render a placeholder. Definition does not render the event at all: "triggers are located by name, not by event."
- Catalog accessors normalise the name they are given (`…-interbase-dialect-and-catalog-design.md` §4.5: "Every singular accessor **normalises the name it is given**; callers pass the identifier text as the user typed it and never upper-case at the call site"). **Never write `strings.ToUpper` at a lookup site in this plan.**
- `ObjectKindFunction` always returns `ErrUnsupportedDDL` (contract D6), and external functions are out of scope for definition entirely. Only `ObjectKindProcedure`, `ObjectKindView` and `ObjectKindTrigger` are ever passed to `ObjectDDL` here.
- No new configuration keys. "Every feature here is automatic when the driver is InterBase and the capability is present."
- **Deliberately not built:** navigation to a line inside a procedure body, `textDocument/references` over catalog source, catalog-change invalidation, and any write-back path. Do not add them.
- Verification commands: `go test ./...`, `make test-race`, and for tagged code `CGO_ENABLED=1 go build -tags interbase ./...`.

## File Structure

| File | Status | Responsibility |
| --- | --- | --- |
| `internal/handler/snapshot_store.go` | Create | `sourceSnapshotStore`, `snapshotContext`, root resolution, directory naming, name escaping, pruning, `write`, `RemoveAll`. Pure filesystem: imports neither `internal/database` nor `internal/lsp` |
| `internal/handler/snapshot_store_test.go` | Create | escaping, permissions, layout, overwrite, prune and removal tests, all rooted at `t.TempDir()` |
| `internal/handler/interbase_definition.go` | Create | target resolution, body/note selection, banner rendering, range computation, `(*Server).interBaseDefinition` |
| `internal/handler/interbase_definition_test.go` | Create | resolution, rendering and end-to-end definition tests |
| `internal/handler/definition.go` | Modify (`handleDefinition`) | fall through to the snapshot path when alias resolution yields nothing |
| `internal/handler/handler.go` | Modify (struct, `NewServer`, `Stop`, `reconnectionDB`) | `snapshots` field, `connGeneration` field, `snapshotContext()` accessor, cleanup on shutdown |
| `doc/develop.md` | Modify (concurrency audit table) | classify the two new `Server` fields |
| `README.md` | Modify (InterBase section) | where snapshots live, their permissions, that editing them does nothing, and the 24-hour crash exposure |

**The spec's §0 file table names a single `internal/handler/interbase_definition.go`.** It is split in two here on purpose: the store is a filesystem lifecycle with its own mutex, prune policy and permission rules, and it is testable — and *must* be tested — without any LSP or database machinery. Keeping it in one file with the LSP feature would mean every store test dragging in a `Server`.

**Two collision notes, because Plan 3 is in flight on the same files:**

1. **`Server.connGeneration` and `(*Server).connectionGeneration()`.** Spec §6.1c lists "DDL memo (§4), snapshot store generation (§5)" as *one* row of the `stateMu` audit table, and §4 keys the hover memo on "an `int` bumped by `reconnectionDB`" — the same counter this plan needs. **Neither plan owns it.** Task 3 Step 3 begins by grepping for `connGeneration`; if Plan 3 has already added it, that step consumes the existing field and adds nothing. If this plan lands first, Plan 3 must consume `connGeneration` rather than adding a second counter. The name is fixed here so both plans can converge on it without coordination.
2. **The README heading `#### InterBase editor features`.** Plan 3 adds it for features 1–4. Task 7 nests under it when it exists and creates it when it does not.

No other shared helper is introduced. In particular this plan does **not** add or consume `configureInterBaseCapabilityServer` (Plan 3's shared fixture from the spec's testing strategy): its tests build their own narrow fixtures, so the two plans cannot collide on test setup.

**What is not reachable end-to-end, stated rather than papered over.** `handleDefinition` obtains its repository from `s.newDBRepository`, which goes through `database.CreateRepository` and the package-level `driverFactories` map. `internal/database/interbase_common.go:17` already registers a factory for `dialect.DatabaseDriverInterBase`, and `RegisterFactory` **panics** on a duplicate registration (`internal/database/driver.go:52-57`), so a test cannot install a capability-bearing repository under the InterBase driver name. The snapshot path is therefore driven by calling `(*Server).interBaseDefinition` directly with an injected repository and cache — which is exactly why that method takes both as parameters instead of reading them off the server. What goes through a real `jsonrpc2` round trip is the alias path and the degradation path (Task 6, Steps 5–8). This is a testability limit of the existing driver registry, not of the design.

---

### Task 1: The snapshot store — root, layout, escaping and permissions

Spec §5, "Store", "URI scheme", "Directory layout", "File naming". This is the first code in sqls that writes a file from the server process; nothing in the repository does today (`main.go:68`'s `os.MkdirAll` is in the interactive `sqls config` subcommand, not in the server). The store is built first and alone, with no database and no LSP types, so its filesystem contract is pinned before anything depends on it.

`kind` is a plain `string` here, not `database.ObjectKind`. That keeps this file — and Tasks 1–3 — free of any dependency on sub-project 2, and keeps the store a filesystem component rather than a catalog component. The caller converts.

**Files:**
- Create: `internal/handler/snapshot_store.go`
- Test: `internal/handler/snapshot_store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type snapshotContext struct { generation int; label string; identity string }`
  - `type sourceSnapshotStore struct { root string; now func() time.Time; … }`
  - `func newSourceSnapshotStore(root string) *sourceSnapshotStore`
  - `func defaultSnapshotRoot() (string, error)`
  - `func (s *sourceSnapshotStore) write(sc snapshotContext, kind, name, content string) (string, error)` — returns the absolute path of the written file.
  - `func escapeSnapshotName(name string) string`
  - `func snapshotDirName(identity string, pid int) string`
  - `const snapshotDirMode os.FileMode = 0o700`, `const snapshotFileMode os.FileMode = 0o600`

- [ ] **Step 1: Write the failing tests**

Create `internal/handler/snapshot_store_test.go`:

```go
package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestSnapshotStore builds a store rooted at a temporary directory with a
// fixed clock.
//
// Every store test in this package must go through this helper. The store the
// server builds in NewServer is rooted at the real user cache directory, and a
// test that wrote or pruned through it would create and delete files in the
// developer's home directory. t.Setenv("XDG_CACHE_HOME", …) is deliberately not
// used: it only steers os.UserCacheDir on Linux and BSD, so on macOS it would
// not redirect anything and CI, which is ubuntu-only, would never notice.
func newTestSnapshotStore(t *testing.T) *sourceSnapshotStore {
	t.Helper()
	store := newSourceSnapshotStore(t.TempDir())
	store.now = func() time.Time {
		return time.Date(2026, 9, 19, 10, 4, 11, 0, time.UTC)
	}
	return store
}

func testSnapshotContext() snapshotContext {
	return snapshotContext{
		generation: 0,
		label:      "local_ib",
		identity:   "interbase|localhost/3050:/db/app.ib||0||APP",
	}
}

func TestSnapshotStoreWritesUnderConnectionAndKindDirectories(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "MYPROC", "-- banner\nBEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	if !filepath.IsAbs(path) {
		t.Errorf("snapshot path %q is not absolute", path)
	}
	wantDir := filepath.Join(store.root, snapshotDirName(sc.identity, os.Getpid()), "procedure")
	if got := filepath.Dir(path); got != wantDir {
		t.Errorf("snapshot directory = %q, want %q", got, wantDir)
	}
	if got := filepath.Base(path); got != "MYPROC.sql" {
		t.Errorf("snapshot file = %q, want %q", got, "MYPROC.sql")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if got, want := string(content), "-- banner\nBEGIN END\n"; got != want {
		t.Errorf("snapshot content = %q, want %q", got, want)
	}
}

func TestSnapshotStoreNeverWritesTheConnectionIdentityToDisk(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	// The identity can carry a host and a filesystem path. It is hashed into
	// the directory name and must not appear anywhere in the path or the file.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	for _, fragment := range []string{"localhost", "/db/app.ib", sc.identity} {
		if strings.Contains(path, fragment) {
			t.Errorf("snapshot path %q leaks connection fragment %q", path, fragment)
		}
		if strings.Contains(string(content), fragment) {
			t.Errorf("snapshot content leaks connection fragment %q", fragment)
		}
	}
}

func TestSnapshotStoreUsesRestrictivePermissions(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("Stat file:", err)
	}
	if got := info.Mode().Perm(); got != snapshotFileMode {
		t.Errorf("snapshot file mode = %#o, want %#o", got, snapshotFileMode)
	}

	// Every directory from the store root down must be 0700: the kind
	// directory, the per-connection directory and the root itself.
	connDir := filepath.Join(store.root, snapshotDirName(sc.identity, os.Getpid()))
	for _, dir := range []string{filepath.Dir(path), connDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal("Stat directory:", err)
		}
		if got := info.Mode().Perm(); got != snapshotDirMode {
			t.Errorf("directory %q mode = %#o, want %#o", dir, got, snapshotDirMode)
		}
	}
}

func TestSnapshotStoreOverwritesAndRestoresPermissions(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "MYPROC", "FIRST\n")
	if err != nil {
		t.Fatal("first write:", err)
	}
	// A snapshot is overwritten on every request, and os.WriteFile applies its
	// mode only when it creates the file. Loosen it and assert the next write
	// tightens it again.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal("Chmod:", err)
	}

	again, err := store.write(sc, "procedure", "MYPROC", "SECOND\n")
	if err != nil {
		t.Fatal("second write:", err)
	}
	if again != path {
		t.Errorf("second write path = %q, want %q", again, path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if got, want := string(content), "SECOND\n"; got != want {
		t.Errorf("snapshot content = %q, want %q — a snapshot is never served stale", got, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("Stat:", err)
	}
	if got := info.Mode().Perm(); got != snapshotFileMode {
		t.Errorf("snapshot file mode after overwrite = %#o, want %#o", got, snapshotFileMode)
	}
}

func TestSnapshotStoreSeparatesConnections(t *testing.T) {
	store := newTestSnapshotStore(t)

	first := testSnapshotContext()
	second := snapshotContext{generation: 1, label: "other", identity: "interbase|other:/db/other.ib"}

	firstPath, err := store.write(first, "procedure", "MYPROC", "FIRST\n")
	if err != nil {
		t.Fatal("first write:", err)
	}
	secondPath, err := store.write(second, "procedure", "MYPROC", "SECOND\n")
	if err != nil {
		t.Fatal("second write:", err)
	}

	if filepath.Dir(firstPath) == filepath.Dir(secondPath) {
		t.Fatal("two connections shared a snapshot directory")
	}
	content, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if got, want := string(content), "FIRST\n"; got != want {
		t.Errorf("first connection snapshot = %q, want %q", got, want)
	}
}

func TestEscapeSnapshotName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "MYPROC", want: "MYPROC"},
		{name: "dollar is escaped", in: "MY$PROC", want: "MY%24PROC"},
		{name: "quote and space", in: `MY$PROC "X"`, want: "MY%24PROC%20%22X%22"},
		{name: "separator", in: "a/b", want: "a%2Fb"},
		{name: "windows separator", in: `a\b`, want: "a%5Cb"},
		{name: "kept punctuation", in: "A-B_C.D", want: "A-B_C.D"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeSnapshotName(tt.in); got != tt.want {
				t.Errorf("escapeSnapshotName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSnapshotStoreEscapesCatalogNames(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", `MY$PROC "X"`, "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	base := filepath.Base(path)
	for _, forbidden := range []string{"$", `"`, " ", "/", `\`} {
		if strings.Contains(base, forbidden) {
			t.Errorf("snapshot file name %q contains an unescaped %q", base, forbidden)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Stat escaped snapshot:", err)
	}
}

func TestSnapshotStoreCannotEscapeItsDirectory(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "../../etc/passwd", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	kindDir := filepath.Join(store.root, snapshotDirName(sc.identity, os.Getpid()), "procedure")
	if got := filepath.Dir(path); got != kindDir {
		t.Fatalf("traversal escaped the kind directory: wrote to %q, want a file directly in %q", path, kindDir)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSnapshotStore|TestEscapeSnapshotName' ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: newSourceSnapshotStore`, `undefined: snapshotContext`, `undefined: snapshotDirName`, `undefined: escapeSnapshotName`, `undefined: snapshotDirMode`, `undefined: snapshotFileMode`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/handler/snapshot_store.go`:

```go
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	snapshotDirMode  os.FileMode = 0o700
	snapshotFileMode os.FileMode = 0o600
)

// snapshotContext identifies the connection a snapshot is written under. It is
// copied out of the Server under stateMu as one value, so the generation and
// the connection identity can never disagree, and so no lock is held across the
// filesystem work that follows.
type snapshotContext struct {
	// generation is the server's connection generation. The store re-derives
	// its directory when it changes.
	generation int
	// label names the connection in the snapshot banner. It is the connection
	// alias, never the attachment string.
	label string
	// identity distinguishes one connection from another. It is hashed into the
	// directory name and never written to disk: it can carry a host and a
	// filesystem path.
	identity string
}

// sourceSnapshotStore materialises database-resident source as read-only files.
//
// The root is a field resolved once at construction, never an os.UserCacheDir()
// call at a use site: that function returns (string, error) and so cannot be
// nested inside filepath.Join, and steering it from tests through
// XDG_CACHE_HOME works only on Linux and BSD. Tests set root directly.
type sourceSnapshotStore struct {
	root string
	// now is injected so the snapshot banner is deterministic in tests.
	now func() time.Time

	mu sync.Mutex
	// pruned records that the one-shot prune has run for this store.
	pruned bool
	// generation and dir memoise the directory derived for that generation, so
	// the hash is computed and the directory created once per connection rather
	// than once per request.
	generation int
	dir        string
	// created is every directory this store made, so shutdown can remove
	// exactly those and nothing a concurrent sqls process owns.
	created map[string]struct{}
}

func newSourceSnapshotStore(root string) *sourceSnapshotStore {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &sourceSnapshotStore{
		root:       root,
		now:        time.Now,
		generation: -1,
		created:    map[string]struct{}{},
	}
}

// defaultSnapshotRoot is where snapshots live when the server resolves the root
// itself. A failure here disables go-to-definition for database-resident
// objects and nothing else.
func defaultSnapshotRoot() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	return filepath.Join(cacheDir, "sqls", "interbase-sources"), nil
}

// write materialises content and returns the absolute path of the file.
//
// The store's own mutex is held across the filesystem calls. That is the point
// of it being a separate lock: stateMu is the one that must never be held
// across I/O, and the caller released it before building the snapshotContext it
// passes here.
func (s *sourceSnapshotStore) write(sc snapshotContext, kind, name, content string) (string, error) {
	if s == nil {
		return "", errors.New("snapshot store is disabled")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir, err := s.connectionDirLocked(sc)
	if err != nil {
		return "", err
	}
	kindDir := filepath.Join(dir, kind)
	if err := os.MkdirAll(kindDir, snapshotDirMode); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}

	path := filepath.Join(kindDir, escapeSnapshotName(name)+".sql")
	if err := os.WriteFile(path, []byte(content), snapshotFileMode); err != nil {
		return "", fmt.Errorf("write snapshot: %w", err)
	}
	// WriteFile applies its mode only when it creates the file, and a snapshot
	// is overwritten on every request.
	if err := os.Chmod(path, snapshotFileMode); err != nil {
		return "", fmt.Errorf("set snapshot permissions: %w", err)
	}
	return path, nil
}

func (s *sourceSnapshotStore) connectionDirLocked(sc snapshotContext) (string, error) {
	if s.dir != "" && s.generation == sc.generation {
		return s.dir, nil
	}
	dir := filepath.Join(s.root, snapshotDirName(sc.identity, os.Getpid()))
	if err := os.MkdirAll(dir, snapshotDirMode); err != nil {
		return "", fmt.Errorf("create snapshot directory: %w", err)
	}
	s.dir = dir
	s.generation = sc.generation
	s.created[dir] = struct{}{}
	return dir, nil
}

// snapshotDirName is <hash>-<pid>. The identity is hashed rather than written
// because it can contain a host and a filesystem path; the pid is in the name
// so pruning can never delete a concurrently running sqls process's live
// directory.
func snapshotDirName(identity string, pid int) string {
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:])[:16], pid)
}

// escapeSnapshotName percent-escapes every byte outside [A-Za-z0-9._-].
//
// This is deliberately not url.PathEscape. RFC 3986 lets `$` stand unescaped in
// a path segment and net/url follows it, so url.PathEscape("MY$PROC") returns
// "MY$PROC" unchanged — and InterBase catalog names contain `$` constantly.
// Escaping conservatively also keeps `/`, `\`, `:` and quotes out of the name,
// so a catalog name can neither escape the kind directory nor produce a path a
// filesystem refuses.
func escapeSnapshotName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestSnapshotStore|TestEscapeSnapshotName' ./internal/handler/ -v`
Expected: every test PASS.

If `TestSnapshotStoreUsesRestrictivePermissions` fails with `mode = 0755`, the modes were written as `0o755`/`0o644`; that is the naive shape this test exists to reject. If `TestSnapshotStoreOverwritesAndRestoresPermissions` fails on the mode but passes on the content, the `os.Chmod` after `os.WriteFile` is missing.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/snapshot_store.go internal/handler/snapshot_store_test.go
git commit -m "feat: add the read-only source snapshot store"
```

---

### Task 2: Prune stale snapshot directories

Spec §5, "Lifetime": "At server start, sibling directories with an mtime older than 24 hours are removed… mtime pruning rather than PID-liveness probing, because it is portable, needs no signals, and cannot delete a concurrent instance's live directory (which is why the PID is in the name)."

**One deliberate divergence from the spec's "at server start".** Pruning runs once per store, lazily, immediately before the first snapshot is written — not in `NewServer`. `NewServer` is called by every test in `internal/handler`, and a prune in the constructor would walk and delete inside the developer's real `~/.cache/sqls/interbase-sources` on every `go test ./...`. Deferring it to the first write means a server that never serves a database-backed definition never touches the directory at all, and the bound the user is promised — nothing older than 24 hours survives an sqls run that uses the feature — is unchanged.

**Files:**
- Modify: `internal/handler/snapshot_store.go`
- Test: `internal/handler/snapshot_store_test.go`

**Interfaces:**
- Consumes: `newSourceSnapshotStore`, `(*sourceSnapshotStore).write`, `snapshotDirName`, `snapshotContext` (Task 1).
- Produces:
  - `const snapshotMaxAge = 24 * time.Hour`
  - `var snapshotDirPattern = regexp.MustCompile(…)`
  - `func (s *sourceSnapshotStore) pruneLocked()` — unexported, called from `write` under `s.mu`, runs at most once per store.

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/snapshot_store_test.go`:

```go
// backdate makes a directory look older than it is. Each fixture below exists
// to reject a different shortcut: `stale` proves pruning happens at all,
// `fresh` proves the age check is real, `mine` proves a running process's own
// directory survives, and `notes.txt` plus `scratch` prove only directories
// matching the <hash>-<pid> shape are eligible — a store that simply removed
// every old entry under its root would delete a user file that happened to be
// sitting there.
func backdate(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal("Chtimes:", err)
	}
}

func TestSnapshotStorePrunesStaleSnapshotDirectories(t *testing.T) {
	store := newTestSnapshotStore(t)

	stale := filepath.Join(store.root, "0123456789abcdef-4242")
	fresh := filepath.Join(store.root, "fedcba9876543210-4243")
	mine := filepath.Join(store.root, snapshotDirName("someone else", os.Getpid()))
	scratch := filepath.Join(store.root, "scratch")
	for _, dir := range []string{stale, fresh, mine, scratch} {
		if err := os.MkdirAll(dir, snapshotDirMode); err != nil {
			t.Fatal("MkdirAll:", err)
		}
	}
	notes := filepath.Join(store.root, "notes.txt")
	if err := os.WriteFile(notes, []byte("keep me"), snapshotFileMode); err != nil {
		t.Fatal("WriteFile:", err)
	}
	backdate(t, stale)
	backdate(t, mine)
	backdate(t, scratch)
	backdate(t, notes)

	if _, err := store.write(testSnapshotContext(), "procedure", "MYPROC", "BEGIN END\n"); err != nil {
		t.Fatal("write:", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale snapshot directory survived pruning (err=%v)", err)
	}
	for _, kept := range []string{fresh, mine, scratch, notes} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("pruning removed %q, which it must not touch: %v", kept, err)
		}
	}
}

func TestSnapshotStorePrunesAtMostOnce(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	if _, err := store.write(sc, "procedure", "MYPROC", "BEGIN END\n"); err != nil {
		t.Fatal("first write:", err)
	}

	// A directory that becomes stale after the store has already pruned must
	// survive: pruning is a start-up sweep, not a garbage collector that runs
	// on every jump.
	late := filepath.Join(store.root, "00112233445566aa-4244")
	if err := os.MkdirAll(late, snapshotDirMode); err != nil {
		t.Fatal("MkdirAll:", err)
	}
	backdate(t, late)

	if _, err := store.write(sc, "view", "MYVIEW", "SELECT 1\n"); err != nil {
		t.Fatal("second write:", err)
	}
	if _, err := os.Stat(late); err != nil {
		t.Errorf("pruning ran a second time and removed %q: %v", late, err)
	}
}

func TestSnapshotStorePruneToleratesMissingRoot(t *testing.T) {
	store := newSourceSnapshotStore(filepath.Join(t.TempDir(), "not", "created", "yet"))
	store.now = func() time.Time { return time.Date(2026, 9, 19, 10, 4, 11, 0, time.UTC) }

	path, err := store.write(testSnapshotContext(), "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write into a root that does not exist yet:", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Stat:", err)
	}
}
```

Note the age boundary: `snapshotMaxAge` is compared against `store.now()`, and `newTestSnapshotStore` pins that to a fixed 2026 date while `backdate` uses `time.Now()`. The fixed clock is far enough in the past relative to nothing — both fixtures are anchored to real wall-clock mtimes, and `TestSnapshotStorePrunesStaleSnapshotDirectories` therefore needs the prune cutoff to come from the real clock. **Use `time.Now()` in `pruneLocked`, not `s.now()`**: `s.now` exists to pin the *banner* timestamp, and coupling the prune cutoff to it would make the fixed test clock silently reclassify every real directory. This is called out because wiring `s.now()` into the prune is the obvious-looking move and it breaks the test above in a confusing way.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSnapshotStorePrune' ./internal/handler/ -v`
Expected: `TestSnapshotStorePrunesStaleSnapshotDirectories` FAILS with `stale snapshot directory survived pruning (err=<nil>)`. `TestSnapshotStorePrunesAtMostOnce` and `TestSnapshotStorePruneToleratesMissingRoot` PASS already — they are the guards that the prune added in Step 3 does not overreach, and they must stay green afterwards.

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/snapshot_store.go`, add `"log"`, `"regexp"` to the imports and the constant and pattern next to the mode constants:

```go
// snapshotMaxAge bounds how long a crashed process's snapshots survive. It is
// the only thing bounding them: a process that dies without running Stop leaves
// database source on disk until some later sqls run prunes it.
const snapshotMaxAge = 24 * time.Hour

// snapshotDirPattern matches the <hash>-<pid> directories this store owns.
// Pruning removes nothing else: the root is a directory in the user's cache,
// and an entry that does not match this shape is not ours to delete.
var snapshotDirPattern = regexp.MustCompile(`^[0-9a-f]{16}-[0-9]+$`)
```

Add the prune, and call it from `write`:

```go
// pruneLocked removes sibling snapshot directories older than snapshotMaxAge.
//
// It runs once per store, immediately before the first snapshot is written,
// rather than at server start: NewServer runs in every unit test in this
// package, and pruning there would walk and delete inside the developer's real
// cache directory.
//
// The cutoff comes from time.Now rather than s.now. s.now exists to pin the
// banner timestamp in tests; using it here would make a fixed test clock
// reclassify every real directory on disk.
func (s *sourceSnapshotStore) pruneLocked() {
	if s.pruned {
		return
	}
	s.pruned = true

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("sqls: read snapshot root %q: %v", s.root, err)
		}
		return
	}

	self := fmt.Sprintf("-%d", os.Getpid())
	cutoff := time.Now().Add(-snapshotMaxAge)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !snapshotDirPattern.MatchString(name) {
			continue
		}
		if strings.HasSuffix(name, self) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, name)); err != nil {
			log.Printf("sqls: prune stale snapshot directory %q: %v", name, err)
		}
	}
}
```

In `write`, add the call as the first statement after taking the lock, before `connectionDirLocked`:

```go
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked()

	dir, err := s.connectionDirLocked(sc)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestSnapshotStore|TestEscapeSnapshotName' ./internal/handler/ -v`
Expected: every test PASS, including the Task 1 tests.

- [ ] **Step 5: Prove the prune test is not vacuous**

The prune test must reject overreach as well as underreach, so check it against the shortcut a hurried implementation takes. Temporarily replace the two `continue` guards in `pruneLocked` — delete the `entry.IsDir() || !snapshotDirPattern.MatchString(name)` check and the `strings.HasSuffix(name, self)` check — and run:

Run: `go test -run TestSnapshotStorePrunesStaleSnapshotDirectories ./internal/handler/ -v`
Expected: FAIL with `pruning removed …/notes.txt, which it must not touch` and `pruning removed …/scratch` and a removal of the current process's own directory. Restore both guards and re-run to confirm PASS before continuing.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/snapshot_store.go internal/handler/snapshot_store_test.go
git commit -m "feat: prune snapshot directories older than 24 hours"
```

---

### Task 3: Shutdown cleanup, server wiring and the connection generation

Spec §5, "Shutdown cleanup must not depend on `Server.Stop` reaching its end", and §6.1c's audit-table row for the snapshot store generation.

Plan 1 Task 3 already removed the early return from `Stop` for the worker's sake; this task appends snapshot removal to that restructured shape and locks the behaviour in with a test that fails against the original. Removal covers only the directories this store created: the root is shared with any other sqls process, whose live snapshots must survive this one's exit.

`Server.connGeneration` is the counter spec §6.1c places under `stateMu`. **Read the collision note in File Structure before Step 3** — Plan 3's hover memo needs the same field, and whichever plan lands second consumes it rather than adding a second counter.

**Files:**
- Modify: `internal/handler/snapshot_store.go`
- Modify: `internal/handler/handler.go` (struct at `:23-42`, `NewServer`, `Stop`, `reconnectionDB`)
- Modify: `doc/develop.md` (the concurrency audit table Plan 1 Task 10 added)
- Test: `internal/handler/snapshot_store_test.go`

**Interfaces:**
- Consumes: `newSourceSnapshotStore`, `defaultSnapshotRoot`, `(*sourceSnapshotStore).write`, `snapshotContext` (Task 1); Plan 1's `Server.stateMu`, restructured `Server.Stop`, nil-guarded `DBConnection.Close`, and the `failingCloser` type in `internal/handler/dispatch_test.go`.
- Produces:
  - `func (s *sourceSnapshotStore) RemoveAll()` — nil-safe, removes every directory this store created.
  - `Server.snapshots *sourceSnapshotStore` — nil when the root could not be resolved.
  - `Server.connGeneration int` — guarded by `stateMu`, incremented by `reconnectionDB`.
  - `func (s *Server) snapshotContext() snapshotContext` — copies the connection identity out from under `stateMu`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/snapshot_store_test.go`:

```go
func TestSnapshotStoreRemoveAllRemovesOnlyItsOwnDirectories(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}
	mine := filepath.Dir(filepath.Dir(path))

	// Another sqls process's live directory, sitting in the shared root.
	theirs := filepath.Join(store.root, "aabbccddeeff0011-4242")
	if err := os.MkdirAll(theirs, snapshotDirMode); err != nil {
		t.Fatal("MkdirAll:", err)
	}

	store.RemoveAll()

	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("RemoveAll left this store's directory behind (err=%v)", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("RemoveAll deleted another process's directory: %v", err)
	}
	if _, err := os.Stat(store.root); err != nil {
		t.Errorf("RemoveAll deleted the shared root: %v", err)
	}
}

func TestSnapshotStoreRemoveAllIsSafeOnANilStore(t *testing.T) {
	var store *sourceSnapshotStore
	store.RemoveAll()
}

func TestSnapshotsRemovedOnShutdown(t *testing.T) {
	server := NewServer()
	store := newTestSnapshotStore(t)
	server.snapshots = store

	path, err := store.write(server.snapshotContext(), "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	if err := server.Stop(); err != nil {
		t.Fatal("Stop:", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("snapshot survived a clean shutdown (err=%v)", err)
	}
}

func TestSnapshotsRemovedEvenWhenConnectionCloseFails(t *testing.T) {
	closeErr := errors.New("attachment is half dead")
	server := NewServer()
	server.dbConn = &database.DBConnection{
		Driver: "stub",
		Tunnel: failingCloser{err: closeErr},
	}
	store := newTestSnapshotStore(t)
	server.snapshots = store

	path, err := store.write(server.snapshotContext(), "procedure", "MYPROC", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}

	// Stop must report the close failure *and* still clean up. A half-dead
	// InterBase attachment is exactly the shutdown that fails, and it must not
	// be the shutdown that leaves database source on disk.
	if err := server.Stop(); !errors.Is(err, closeErr) {
		t.Fatalf("Stop() = %v, want %v", err, closeErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("snapshot survived a failing shutdown (err=%v)", err)
	}
	select {
	case <-server.worker.Done():
	default:
		t.Error("Stop returned without stopping the worker")
	}
}

func TestSnapshotContextTracksTheConnectionGeneration(t *testing.T) {
	server := NewServer()
	defer server.worker.Stop()

	server.curDBCfg = &database.DBConfig{
		Alias:          "local_ib",
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: "localhost/3050:/db/app.ib",
	}

	sc := server.snapshotContext()
	if sc.label != "local_ib" {
		t.Errorf("label = %q, want %q", sc.label, "local_ib")
	}
	if !strings.Contains(sc.identity, "localhost/3050:/db/app.ib") {
		t.Errorf("identity = %q, want it to include the data source name", sc.identity)
	}

	server.stateMu.Lock()
	server.connGeneration++
	server.stateMu.Unlock()

	if got := server.snapshotContext().generation; got != sc.generation+1 {
		t.Errorf("generation after a reconnect = %d, want %d", got, sc.generation+1)
	}
}

func TestSnapshotContextWithoutAConnection(t *testing.T) {
	server := NewServer()
	defer server.worker.Stop()

	sc := server.snapshotContext()
	if sc.label == "" {
		t.Error("label is empty; the banner must always name something")
	}
}
```

Add `"errors"`, `"github.com/sqls-server/sqls/dialect"` and `"github.com/sqls-server/sqls/internal/database"` to the file's imports. `failingCloser` comes from `internal/handler/dispatch_test.go` (Plan 1 Task 3) and is not redeclared here.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSnapshot' ./internal/handler/ -v`
Expected: FAIL to compile — `store.RemoveAll undefined`, `server.snapshots undefined`, `server.snapshotContext undefined`, `server.connGeneration undefined`.

- [ ] **Step 3: Write the minimal implementation**

First check whether Plan 3 has already added the generation counter:

Run: `grep -rn 'connGeneration' internal/handler --include='*.go'`

If it already exists on `Server` and is already incremented in `reconnectionDB`, **skip those two edits below** and add only `snapshots`, `RemoveAll`, `snapshotContext()` and the `Stop` change.

In `internal/handler/snapshot_store.go`, add:

```go
// RemoveAll removes every directory this store created. It is safe on a nil
// store, which is what a server whose cache directory could not be located has.
//
// It removes only its own directories, never the root: the root is shared with
// any other sqls process, whose live snapshots must survive this one's exit.
// The lock is released before the filesystem work, so a shutdown never waits on
// an in-flight write beyond the bookkeeping.
func (s *sourceSnapshotStore) RemoveAll() {
	if s == nil {
		return
	}

	s.mu.Lock()
	dirs := make([]string, 0, len(s.created))
	for dir := range s.created {
		dirs = append(dirs, dir)
	}
	s.created = map[string]struct{}{}
	s.dir = ""
	s.generation = -1
	s.mu.Unlock()

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("sqls: remove snapshot directory %q: %v", dir, err)
		}
	}
}
```

In `internal/handler/handler.go`, add the two fields to `Server`, below the existing mutable fields and above `worker`:

```go
	// connGeneration advances on every reconnect. It ties per-connection
	// artefacts — the snapshot store's directory, and the hover DDL memo — to
	// the connection they were produced under. Guarded by stateMu.
	connGeneration int

	// snapshots materialises database-resident source as read-only files. It is
	// nil when the user cache directory could not be located, which disables
	// go-to-definition for database objects and nothing else.
	snapshots *sourceSnapshotStore
```

In `NewServer`, build the store after the struct literal:

```go
func NewServer() *Server {
	worker := database.NewWorker()
	worker.Start()

	server := &Server{
		files:   make(map[string]*File),
		worker:  worker,
		cancels: newCancelRegistry(),
	}
	// Deliberately no filesystem access here: NewServer runs in every test in
	// this package, and touching the real cache directory from a unit test is
	// the hazard the injected root exists to remove. The root is only resolved,
	// never created or read, until the first snapshot is written.
	root, err := defaultSnapshotRoot()
	if err != nil {
		log.Printf("sqls: go-to-definition snapshots are disabled: %v", err)
	} else {
		server.snapshots = newSourceSnapshotStore(root)
	}
	return server
}
```

Replace `Stop` with Plan 1's shape plus the cleanup:

```go
// Stop closes the database connection, always stops the worker, and always
// removes this process's snapshots — including when closing the connection
// fails. A half-dead InterBase attachment is exactly the shutdown that fails,
// and it must not be the one that leaves database source on disk.
//
// It deliberately takes no connMu — a runaway query must not be able to hold
// the process open — but it does take stateMu for the pointer read, because a
// concurrent switch may be reassigning it.
func (s *Server) Stop() error {
	defer s.snapshots.RemoveAll()
	defer s.worker.Stop()
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	return dbConn.Close()
}
```

In `reconnectionDB`, extend Plan 1's guarded assignment:

```go
	s.stateMu.Lock()
	s.dbConn = dbConn
	s.connGeneration++
	s.stateMu.Unlock()
```

Add the accessor below `parserDriver`:

```go
// snapshotContext copies the connection identity out from under stateMu.
// Everything the snapshot store does with the result is filesystem I/O, which
// stateMu is never held across.
func (s *Server) snapshotContext() snapshotContext {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	sc := snapshotContext{generation: s.connGeneration, label: "(unnamed)"}
	cfg := s.curDBCfg
	if cfg == nil {
		return sc
	}
	if cfg.Alias != "" {
		sc.label = cfg.Alias
	}
	// Every field that can distinguish one attachment from another goes into
	// the hash. Over-inclusion is free because the result is hashed and never
	// displayed, and it guarantees two different connections never share a
	// snapshot directory.
	sc.identity = fmt.Sprintf("%s|%s|%s|%d|%s|%s",
		cfg.Driver, cfg.DataSourceName, cfg.Host, cfg.Port, cfg.Path, cfg.DBName)
	return sc
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestSnapshot' ./internal/handler/ -v`
Expected: every test PASS.

- [ ] **Step 5: Prove the failing-shutdown test is not vacuous**

`TestSnapshotsRemovedEvenWhenConnectionCloseFails` is a regression lock on the `Stop` shape Plan 1 established, so check it against the shape that shape replaced. Temporarily restore the original body:

```go
func (s *Server) Stop() error {
	if err := s.dbConn.Close(); err != nil {
		return err
	}
	s.worker.Stop()
	s.snapshots.RemoveAll()
	return nil
}
```

Run: `go test -run 'TestSnapshotsRemoved' ./internal/handler/ -v`
Expected: `TestSnapshotsRemovedEvenWhenConnectionCloseFails` FAILS with `snapshot survived a failing shutdown (err=<nil>)` and `Stop returned without stopping the worker`, while `TestSnapshotsRemovedOnShutdown` still PASSES — which is the whole point: a cleanup test that only exercises the happy path passes against both shapes and proves nothing. Restore the deferred form and re-run to confirm both PASS.

- [ ] **Step 6: Record the new fields in the concurrency audit table**

Plan 1's Global Constraints require it: "Any new `Server` field added by a later plan must be added to the audit table in `doc/develop.md` and classified." In `doc/develop.md`, add two rows to that table:

| Field | Written by | Read by | Treatment |
| --- | --- | --- | --- |
| `connGeneration` | `reconnectionDB` | `snapshotContext` (inline, definition) | `stateMu` |
| `snapshots` | `NewServer` only | `interBaseDefinition` (inline), `Stop` | pointer never reassigned after construction; the store guards its own state with its own mutex, which **is** held across filesystem I/O — it is a leaf lock, unlike `stateMu` |

- [ ] **Step 7: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/handler/snapshot_store.go internal/handler/snapshot_store_test.go internal/handler/handler.go doc/develop.md
git commit -m "feat: remove snapshots on shutdown even when the connection close fails"
```

---

### Task 4: Resolve the identifier under the cursor to a catalog object

Spec §5, "Resolution order". `definition.go` today resolves *in-document aliases and subquery names only*: `definitionWithDriver` calls `parseutil.ExtractAliased` (`:60`), returns `nil, nil` when the document has no aliases at all (`:62`), and matches with a string comparison (`:68`). No repository is reachable from it — `handleDefinition:34` passes only the cache. This task adds the first database-backed resolution; it does not touch the alias path.

**This is the first task that depends on sub-project 2.** Everything before it is filesystem work; everything after it needs the contract.

**Files:**
- Create: `internal/handler/interbase_definition.go`
- Test: `internal/handler/interbase_definition_test.go`

**Interfaces:**
- Consumes: from sub-project 2's contract — `database.ObjectKind` with `ObjectKindProcedure`/`ObjectKindView`/`ObjectKindTrigger`, `(*database.DBCache).HasCatalog/Procedure/View/Trigger`, `database.CatalogCache`, `database.ProcedureDesc.Source`, `database.ViewDesc.ViewSource`, `database.TriggerDesc.Source`.
- Produces:
  - `type snapshotTarget struct { kind database.ObjectKind; name string; source sql.NullString }`
  - `func resolveSnapshotTarget(text string, params lsp.DefinitionParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (snapshotTarget, bool)`

- [ ] **Step 1: Verify sub-project 2's contract has landed**

Run:

```bash
grep -n 'func (dc \*DBCache) HasCatalog\|func (dc \*DBCache) Procedure\|func (dc \*DBCache) View\|func (dc \*DBCache) Trigger' internal/database/cache.go
grep -n 'ObjectKindProcedure\|ObjectKindView\|ObjectKindTrigger\|ErrObjectNotFound\|ErrUnsupportedDDL\|func UnsupportedDDLDetail\|type DDLRepository' internal/database/capability.go
grep -n 'type CatalogCache\|Procedures \|Views \|Triggers ' internal/database/cache.go
```

Expected: every name found, and `CatalogCache` carrying `Views`, `Procedures` and `Triggers` maps keyed by name. If any is missing, **stop**: sub-project 2's catalog plan has not landed and Tasks 4–6 cannot be built. Tasks 1–3 and 7 are unaffected and can ship on their own.

- [ ] **Step 2: Write the failing tests**

Create `internal/handler/interbase_definition_test.go`:

```go
package handler

import (
	"database/sql"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// definitionCatalog is the shared catalog fixture: one procedure with a
// verbatim body, one view with a verbatim body, one trigger whose Event is ""
// (the normal case until the driver-side accessor spec lands), and one
// procedure whose Source is invalid so the "nothing to write" path has a
// fixture.
func definitionCatalog() *database.DBCache {
	return &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"MYPROC": {
					Name:   "MYPROC",
					Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				},
				"NOSOURCE": {
					Name:   "NOSOURCE",
					Source: sql.NullString{},
				},
			},
			Views: map[string]*database.ViewDesc{
				"MYVIEW": {
					Name:       "MYVIEW",
					ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				},
			},
			Triggers: map[string]*database.TriggerDesc{
				"MYTRIGGER": {
					Name:   "MYTRIGGER",
					Event:  "",
					Source: sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
				},
			},
		},
	}
}

func definitionParamsAt(character int) lsp.DefinitionParams {
	return lsp.DefinitionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: character},
		},
	}
}

func TestResolveSnapshotTarget(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		character  int
		driver     dialect.DatabaseDriver
		dbCache    *database.DBCache
		wantOK     bool
		wantKind   database.ObjectKind
		wantName   string
		wantSource string
	}{
		{
			name:       "procedure",
			text:       "execute procedure myproc",
			character:  20,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindProcedure,
			wantName:   "MYPROC",
			wantSource: "BEGIN\n  SUSPEND;\nEND",
		},
		{
			name:       "view",
			text:       "select * from myview",
			character:  16,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindView,
			wantName:   "MYVIEW",
			wantSource: "SELECT ID FROM CITY",
		},
		{
			name:       "trigger reached by the identifier under the cursor",
			text:       "select mytrigger",
			character:  10,
			driver:     dialect.DatabaseDriverInterBase,
			dbCache:    definitionCatalog(),
			wantOK:     true,
			wantKind:   database.ObjectKindTrigger,
			wantName:   "MYTRIGGER",
			wantSource: "BEGIN\n  NEW.ID = 1;\nEND",
		},
		{
			name:      "unknown identifier",
			text:      "select * from nosuchthing",
			character: 16,
			driver:    dialect.DatabaseDriverInterBase,
			dbCache:   definitionCatalog(),
			wantOK:    false,
		},
		{
			name:      "no catalog yet",
			text:      "execute procedure myproc",
			character: 20,
			driver:    dialect.DatabaseDriverInterBase,
			dbCache:   &database.DBCache{},
			wantOK:    false,
		},
		{
			name:      "not interbase",
			text:      "execute procedure myproc",
			character: 20,
			driver:    dialect.DatabaseDriverPostgreSQL,
			dbCache:   definitionCatalog(),
			wantOK:    false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveSnapshotTarget(tt.text, definitionParamsAt(tt.character), tt.dbCache, tt.driver)
			if ok != tt.wantOK {
				t.Fatalf("resolveSnapshotTarget ok = %v, want %v (target=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tt.wantKind)
			}
			// The catalog spelling, not the user's: it is what the banner and
			// the file name use.
			if got.name != tt.wantName {
				t.Errorf("name = %q, want %q", got.name, tt.wantName)
			}
			if got.source.String != tt.wantSource {
				t.Errorf("source = %q, want %q", got.source.String, tt.wantSource)
			}
		})
	}
}

func TestResolveSnapshotTargetIsCaseInsensitiveWithoutUpperCasingAtTheCallSite(t *testing.T) {
	// The accessors normalise the name they are given. This test fails if the
	// call site starts upper-casing, because then a mixed-case identifier would
	// still work and a broken accessor would go unnoticed — and it fails if the
	// accessors are exact-match, which is the D11 risk the contract flags.
	got, ok := resolveSnapshotTarget("execute procedure MyProc", definitionParamsAt(20), definitionCatalog(), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("a mixed-case identifier did not resolve; DBCache accessors must normalise the name they are given")
	}
	if got.name != "MYPROC" {
		t.Errorf("name = %q, want %q", got.name, "MYPROC")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestResolveSnapshotTarget' ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: resolveSnapshotTarget`.

- [ ] **Step 4: Write the minimal implementation**

Create `internal/handler/interbase_definition.go`:

```go
package handler

import (
	"database/sql"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// snapshotTarget is a catalog object go-to-definition can materialise. source
// is the descriptor's verbatim catalog text, carried along because it is the
// body used when the catalog cannot reproduce executable DDL.
type snapshotTarget struct {
	kind   database.ObjectKind
	name   string
	source sql.NullString
}

// resolveSnapshotTarget classifies the identifier under the cursor against the
// catalog. It is pure: no I/O, no server state.
//
// Triggers are resolved here although they are excluded from completion. They
// are never named in DML, so there is no completion context for them, but the
// identifier under the cursor is a perfectly good way to reach one.
func resolveSnapshotTarget(text string, params lsp.DefinitionParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (snapshotTarget, bool) {
	if driver != dialect.DatabaseDriverInterBase || !dbCache.HasCatalog() {
		return snapshotTarget{}, false
	}

	parsed, err := parser.ParseWithDriver(text, driver)
	if err != nil {
		return snapshotTarget{}, false
	}
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	ident := nodeWalker.CurNodeBottomMatched(astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeIdentifier},
	})
	if ident == nil {
		return snapshotTarget{}, false
	}

	// The accessors normalise the name they are given, so the identifier text
	// goes in exactly as the user typed it. Never upper-case here.
	name := ident.String()
	if desc, ok := dbCache.Procedure(name); ok {
		return snapshotTarget{kind: database.ObjectKindProcedure, name: desc.Name, source: desc.Source}, true
	}
	if desc, ok := dbCache.View(name); ok {
		return snapshotTarget{kind: database.ObjectKindView, name: desc.Name, source: desc.ViewSource}, true
	}
	if desc, ok := dbCache.Trigger(name); ok {
		return snapshotTarget{kind: database.ObjectKindTrigger, name: desc.Name, source: desc.Source}, true
	}
	return snapshotTarget{}, false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestResolveSnapshotTarget' ./internal/handler/ -v`
Expected: every subtest PASS.

If the mixed-case subtest fails, the `DBCache` accessors are exact-match rather than normalising. That is D11's flagged risk, and the fix is a one-line change in sub-project 2's accessors (`strings.ToUpper` on the argument, matching `columnDatabaseKey` at `internal/database/cache.go:186-188`). **Do not work around it here**: hover, signature help and procedure routing all depend on the same normalisation, and patching one call site hides the bug from the other three.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/interbase_definition.go internal/handler/interbase_definition_test.go
git commit -m "feat: resolve the identifier under the cursor to a catalog object"
```

---

### Task 5: Body selection, the banner and the range

Spec §5, "Content", "Body selection", "Range", and the "DDL is unsupported (go-to-definition)" entry of User-Visible Behavior.

Four outcomes, one of which writes a file and three of which write nothing. Getting this wrong is how a stale cache entry leaves a banner-only file on disk, or how a fabricated `CREATE` header ends up in front of a body that is not a declaration.

**Files:**
- Modify: `internal/handler/interbase_definition.go`
- Test: `internal/handler/interbase_definition_test.go`

**Interfaces:**
- Consumes: `snapshotTarget` (Task 4); `snapshotContext` (Task 1); `database.ErrObjectNotFound`, `database.ErrUnsupportedDDL`, `database.UnsupportedDDLDetail` (sub-project 2); `utf16Len` (`internal/handler/execute_command.go:211`).
- Produces:
  - `func snapshotBodyFor(ddl string, ddlErr error, target snapshotTarget) (body, note string, ok bool)`
  - `func renderSnapshot(target snapshotTarget, sc snapshotContext, now time.Time, body, note string) (content string, bannerLines int)`
  - `func snapshotRange(content string, bannerLines int, name string) lsp.Range`
  - `func commentLines(text string) string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/interbase_definition_test.go`:

```go
// unsupportedDDLError is a local stand-in for the driver's structured
// unsupported-DDL error. It satisfies errors.Is against
// database.ErrUnsupportedDDL and exposes the detail through the same
// unexported-interface shape database.UnsupportedDDLDetail matches on, so no
// error string is ever compared.
type unsupportedDDLError struct {
	object  string
	name    string
	feature string
}

func (e *unsupportedDDLError) Error() string {
	return "stub: DDL is unavailable for this object"
}

func (e *unsupportedDDLError) Unwrap() error { return database.ErrUnsupportedDDL }

func (e *unsupportedDDLError) UnsupportedDDLDetail() (string, string, string) {
	return e.object, e.name, e.feature
}

func testSnapshotTarget(kind database.ObjectKind, name, source string) snapshotTarget {
	target := snapshotTarget{kind: kind, name: name}
	if source != "" {
		target.source = sql.NullString{String: source, Valid: true}
	}
	return target
}

func TestSnapshotBodyForSuccessUsesTheGeneratedDDL(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	body, note, ok := snapshotBodyFor(`CREATE PROCEDURE "MYPROC" AS BEGIN END`, nil, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != `CREATE PROCEDURE "MYPROC" AS BEGIN END` {
		t.Errorf("body = %q, want the generated DDL", body)
	}
	if note != "" {
		t.Errorf("note = %q, want no note on success", note)
	}
}

func TestSnapshotBodyForUnsupportedDDLUsesTheVerbatimSource(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN\n  SUSPEND;\nEND")
	err := &unsupportedDDLError{
		object:  "procedure",
		name:    "MYPROC",
		feature: `parameter "IN_AMOUNT" nullability is unknown`,
	}

	body, note, ok := snapshotBodyFor("", err, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != "BEGIN\n  SUSPEND;\nEND" {
		t.Errorf("body = %q, want the verbatim catalog source", body)
	}
	if strings.Contains(body, "CREATE") {
		t.Error("body contains a synthesized CREATE header; a declaration must never be fabricated")
	}
	for _, want := range []string{"procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to name %q", note, want)
		}
	}
}

func TestSnapshotBodyForUnsupportedDDLWithoutDetailDegrades(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindView, "MYVIEW", "SELECT ID FROM CITY")

	body, note, ok := snapshotBodyFor("", database.ErrUnsupportedDDL, target)
	if !ok {
		t.Fatal("snapshotBodyFor ok = false, want true")
	}
	if body != "SELECT ID FROM CITY" {
		t.Errorf("body = %q, want the verbatim view source", body)
	}
	if note == "" {
		t.Error("note is empty; the reader must be told the body is not executable DDL")
	}
	if strings.Contains(note, "stub:") {
		t.Errorf("note = %q, want no driver message text", note)
	}
}

func TestSnapshotBodyForWritesNothing(t *testing.T) {
	cases := []struct {
		name   string
		target snapshotTarget
		err    error
	}{
		{
			name:   "object not found",
			target: testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END"),
			err:    database.ErrObjectNotFound,
		},
		{
			name:   "unsupported DDL and no source",
			target: testSnapshotTarget(database.ObjectKindProcedure, "NOSOURCE", ""),
			err:    &unsupportedDDLError{object: "procedure", name: "NOSOURCE", feature: "nullability is unknown"},
		},
		{
			name:   "any other error",
			target: testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END"),
			err:    errors.New("connection reset"),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			body, note, ok := snapshotBodyFor("", tt.err, tt.target)
			if ok {
				t.Fatalf("snapshotBodyFor ok = true, want false (body=%q note=%q)", body, note)
			}
		})
	}
}

func TestRenderSnapshotBanner(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	sc := testSnapshotContext()
	now := time.Date(2026, 9, 19, 10, 4, 11, 0, time.UTC)

	content, bannerLines := renderSnapshot(target, sc, now, "BEGIN\n  SUSPEND;\nEND", "")

	want := `-- sqls: read-only snapshot of InterBase PROCEDURE "MYPROC"
-- connection: local_ib    generated: 2026-09-19T10:04:11Z
-- Editing this file does not change the database.
BEGIN
  SUSPEND;
END
`
	if content != want {
		t.Errorf("content =\n%q\nwant\n%q", content, want)
	}
	if bannerLines != 3 {
		t.Errorf("bannerLines = %d, want 3", bannerLines)
	}
	if strings.Contains(content, sc.identity) {
		t.Error("the banner leaks the connection identity")
	}
}

func TestRenderSnapshotKeepsTheNoteInsideComments(t *testing.T) {
	target := testSnapshotTarget(database.ObjectKindProcedure, "MYPROC", "BEGIN END")
	// A detail string that contains a newline must not break out of the comment
	// and turn into executable-looking text.
	note := "Executable DDL could not be reproduced: procedure\nsomething else entirely"

	content, bannerLines := renderSnapshot(target, testSnapshotContext(), time.Unix(0, 0).UTC(), "BEGIN END", note)

	lines := strings.Split(content, "\n")
	if bannerLines != 5 {
		t.Fatalf("bannerLines = %d, want 5 (3 banner + 2 note lines)", bannerLines)
	}
	for i := 0; i < bannerLines; i++ {
		if !strings.HasPrefix(lines[i], "--") {
			t.Errorf("line %d = %q, want a comment", i, lines[i])
		}
	}
}

func TestSnapshotRange(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		bannerLines int
		object      string
		want        lsp.Range
	}{
		{
			name:        "first occurrence after the banner",
			content:     "-- sqls: MYPROC\n-- b\n-- c\nCREATE PROCEDURE \"MYPROC\" AS\nBEGIN END\n",
			bannerLines: 3,
			object:      "MYPROC",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 18},
				End:   lsp.Position{Line: 3, Character: 18},
			},
		},
		{
			name:        "name absent from the body",
			content:     "-- a\n-- b\n-- c\nBEGIN\n  SUSPEND;\nEND\n",
			bannerLines: 3,
			object:      "MYPROC",
			want:        lsp.Range{},
		},
		{
			name:        "character offset is counted in UTF-16 units",
			content:     "-- a\n-- b\n-- c\n/* 𝕏𝕏 */ MYPROC\n",
			bannerLines: 3,
			object:      "MYPROC",
			want: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 11},
				End:   lsp.Position{Line: 3, Character: 11},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapshotRange(tt.content, tt.bannerLines, tt.object); got != tt.want {
				t.Errorf("snapshotRange = %+v, want %+v", got, tt.want)
			}
		})
	}
}
```

The UTF-16 case is worth reading twice: `/* 𝕏𝕏 */ ` is 9 runes but `𝕏` (U+1D54F) is outside the BMP and costs two UTF-16 code units each, so the LSP character offset of `MYPROC` is 11, not 9. A `utf8.RuneCountInString` implementation returns 9 and fails here.

Add `"errors"`, `"strings"` and `"time"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSnapshotBodyFor|TestRenderSnapshot|TestSnapshotRange' ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: snapshotBodyFor`, `undefined: renderSnapshot`, `undefined: snapshotRange`.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/handler/interbase_definition.go`, adding `"errors"`, `"fmt"`, `"log"`, `"strings"` and `"time"` to the imports:

```go
// snapshotBodyFor chooses the snapshot body from the ObjectDDL outcome. A false
// ok means no file may be written at all — not a banner, not an empty body.
func snapshotBodyFor(ddl string, ddlErr error, target snapshotTarget) (body, note string, ok bool) {
	switch {
	case ddlErr == nil:
		return ddl, "", true

	case errors.Is(ddlErr, database.ErrObjectNotFound):
		// The cache named an object the catalog does not have, so the cache is
		// stale. Writing a snapshot of nothing is worse than navigating nowhere.
		return "", "", false

	case errors.Is(ddlErr, database.ErrUnsupportedDDL):
		// The normal case for procedures: schema/README.md states that
		// parameter nullability is usually unknown, and only a non-nullable
		// domain can prove otherwise.
		if !target.source.Valid || target.source.String == "" {
			return "", "", false
		}
		return target.source.String, unsupportedDDLNote(ddlErr), true

	default:
		log.Printf("sqls: object DDL for %s %q: %v", target.kind, target.name, ddlErr)
		return "", "", false
	}
}

// unsupportedDDLNote names what blocked executable DDL, from the structured
// detail and never from the driver's message text. No CREATE header is
// synthesized anywhere: what follows this note is the catalog's own source.
func unsupportedDDLNote(err error) string {
	object, name, feature, ok := database.UnsupportedDDLDetail(err)
	if !ok {
		return "Executable DDL could not be reproduced. The verbatim catalog source follows."
	}
	return fmt.Sprintf("Executable DDL could not be reproduced: %s %q: %s.\nThe verbatim catalog source follows.",
		object, name, feature)
}

// commentLines prefixes every line with "-- ". The detail strings it renders
// come from the driver, so a newline in one must not be able to end the comment
// and leave text that reads like SQL.
func commentLines(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("-- ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderSnapshot builds the file content and reports how many leading lines are
// banner, so the range computation can skip them.
func renderSnapshot(target snapshotTarget, sc snapshotContext, now time.Time, body, note string) (string, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "-- sqls: read-only snapshot of InterBase %s %q\n",
		strings.ToUpper(string(target.kind)), target.name)
	fmt.Fprintf(&b, "-- connection: %s    generated: %s\n",
		sc.label, now.UTC().Format(time.RFC3339))
	b.WriteString("-- Editing this file does not change the database.\n")
	bannerLines := 3

	if note != "" {
		commented := commentLines(note)
		b.WriteString(commented)
		bannerLines += strings.Count(commented, "\n")
	}

	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), bannerLines
}

// snapshotRange is the zero-width range at the first occurrence of the object
// name in the body. The banner is skipped so the name in the banner never wins.
// It is (0,0) when the name does not appear, which happens when the generated
// DDL quotes the name differently than the catalog spells it.
func snapshotRange(content string, bannerLines int, name string) lsp.Range {
	lines := strings.Split(content, "\n")
	for i := bannerLines; i < len(lines); i++ {
		offset := strings.Index(lines[i], name)
		if offset < 0 {
			continue
		}
		// LSP character offsets are UTF-16 code units, not bytes or runes.
		pos := lsp.Position{Line: i, Character: utf16Len(lines[i][:offset])}
		return lsp.Range{Start: pos, End: pos}
	}
	return lsp.Range{}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestSnapshotBodyFor|TestRenderSnapshot|TestSnapshotRange' ./internal/handler/ -v`
Expected: every subtest PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/interbase_definition.go internal/handler/interbase_definition_test.go
git commit -m "feat: render snapshot banners, bodies and name ranges"
```

---

### Task 6: `interBaseDefinition` and the `handleDefinition` fallback

Spec §5, "Plumbing": "`definitionWithDriver` is currently pure and has no repository. Rather than thread a repository through the pure function, `handleDefinition` calls it first and, on an empty result, runs `s.interBaseDefinition(...)`, which does its own resolution, `ObjectDDL` call, and snapshot write."

The repository and the cache are parameters rather than reads off the server, for two reasons: it keeps the method free of `stateMu` beyond the one `snapshotContext()` call, and it is the only way to test the path at all — see "What is not reachable end-to-end" in File Structure.

**Files:**
- Modify: `internal/handler/interbase_definition.go`
- Modify: `internal/handler/definition.go` (`handleDefinition`)
- Test: `internal/handler/interbase_definition_test.go`

**Interfaces:**
- Consumes: `resolveSnapshotTarget` (Task 4); `snapshotBodyFor`, `renderSnapshot`, `snapshotRange` (Task 5); `(*sourceSnapshotStore).write`, `(*Server).snapshotContext` (Tasks 1, 3); `database.DDLRepository.ObjectDDL` (sub-project 2); Plan 1's `(*Server).fileText`.
- Produces:
  - `const definitionDDLTimeout = 3 * time.Second`
  - `func (s *Server) interBaseDefinition(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.DefinitionParams, text string) (lsp.Definition, error)`
  - `func snapshotURI(path string) string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/interbase_definition_test.go`:

```go
// stubDDLRepository is a DBRepository that also answers ObjectDDL. The embedded
// mock's other methods are never called on the definition path, so their func
// fields stay nil.
type stubDDLRepository struct {
	*database.MockDBRepository
	ddl   func(ctx context.Context, kind database.ObjectKind, name string) (string, error)
	calls int
}

func (r *stubDDLRepository) ObjectDDL(ctx context.Context, kind database.ObjectKind, name string) (string, error) {
	r.calls++
	return r.ddl(ctx, kind, name)
}

func newStubDDLRepository(ddl func(context.Context, database.ObjectKind, string) (string, error)) *stubDDLRepository {
	return &stubDDLRepository{MockDBRepository: &database.MockDBRepository{}, ddl: ddl}
}

// newDefinitionServer builds a server wired for the snapshot path: the
// InterBase driver, a connection config to hash, and a store rooted at a
// temporary directory.
//
// The store replacement is mandatory, not cosmetic: NewServer roots its store
// at the real user cache directory, so a test that skipped this line would
// create files in the developer's home directory.
func newDefinitionServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer()
	t.Cleanup(server.worker.Stop)
	server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	server.curDBCfg = &database.DBConfig{
		Alias:          "local_ib",
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: "localhost/3050:/db/app.ib",
	}
	server.snapshots = newTestSnapshotStore(t)
	return server
}

// snapshotRootIsEmpty reports whether the store wrote nothing at all. The root
// itself may not exist, which also counts as empty.
func snapshotRootIsEmpty(t *testing.T, store *sourceSnapshotStore) bool {
	t.Helper()
	var files int
	err := filepath.WalkDir(store.root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("WalkDir:", err)
	}
	return files == 0
}

func TestInterBaseDefinitionProcedureWritesReadOnlySnapshot(t *testing.T) {
	server := newDefinitionServer(t)
	repo := newStubDDLRepository(func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
		if kind != database.ObjectKindProcedure || name != "MYPROC" {
			t.Errorf("ObjectDDL(%q, %q), want (procedure, MYPROC)", kind, name)
		}
		return "CREATE PROCEDURE \"MYPROC\" AS\nBEGIN\n  SUSPEND;\nEND", nil
	})

	got, err := server.interBaseDefinition(context.Background(), repo, definitionCatalog(), definitionParamsAt(20), "execute procedure myproc")
	if err != nil {
		t.Fatal("interBaseDefinition:", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}

	if !strings.HasPrefix(got[0].URI, "file://") {
		t.Errorf("URI = %q, want a file:// URI — a client that cannot open the scheme is worse than no location", got[0].URI)
	}

	path := filepath.Join(server.snapshots.root,
		snapshotDirName(server.snapshotContext().identity, os.Getpid()), "procedure", "MYPROC.sql")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("Stat snapshot:", err)
	}
	if got := info.Mode().Perm(); got != snapshotFileMode {
		t.Errorf("snapshot mode = %#o, want %#o", got, snapshotFileMode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal("Stat snapshot directory:", err)
	}
	if got := dirInfo.Mode().Perm(); got != snapshotDirMode {
		t.Errorf("snapshot directory mode = %#o, want %#o", got, snapshotDirMode)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	for _, want := range []string{
		`-- sqls: read-only snapshot of InterBase PROCEDURE "MYPROC"`,
		"-- connection: local_ib",
		"-- Editing this file does not change the database.",
		`CREATE PROCEDURE "MYPROC" AS`,
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("snapshot does not contain %q:\n%s", want, content)
		}
	}
}

func TestInterBaseDefinitionRangePointsAtObjectName(t *testing.T) {
	server := newDefinitionServer(t)
	repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE \"MYPROC\" AS\nBEGIN END", nil
	})

	got, err := server.interBaseDefinition(context.Background(), repo, definitionCatalog(), definitionParamsAt(20), "execute procedure myproc")
	if err != nil {
		t.Fatal("interBaseDefinition:", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want 1", len(got))
	}
	// Line 3 is the first body line; MYPROC starts at character 18 of
	// `CREATE PROCEDURE "MYPROC" AS`.
	want := lsp.Range{
		Start: lsp.Position{Line: 3, Character: 18},
		End:   lsp.Position{Line: 3, Character: 18},
	}
	if got[0].Range != want {
		t.Errorf("range = %+v, want %+v", got[0].Range, want)
	}
}

func TestInterBaseDefinitionUnsupportedDDLUsesVerbatimSource(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		character  int
		kind       string
		file       string
		wantBody   string
		wantObject string
	}{
		{
			name:       "procedure",
			text:       "execute procedure myproc",
			character:  20,
			kind:       "procedure",
			file:       "MYPROC.sql",
			wantBody:   "BEGIN\n  SUSPEND;\nEND",
			wantObject: "MYPROC",
		},
		{
			name:       "view",
			text:       "select * from myview",
			character:  16,
			kind:       "view",
			file:       "MYVIEW.sql",
			wantBody:   "SELECT ID FROM CITY",
			wantObject: "MYVIEW",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
				return "", &unsupportedDDLError{
					object:  tt.kind,
					name:    tt.wantObject,
					feature: `parameter "IN_AMOUNT" nullability is unknown`,
				}
			})

			got, err := server.interBaseDefinition(context.Background(), repo, definitionCatalog(), definitionParamsAt(tt.character), tt.text)
			if err != nil {
				t.Fatal("interBaseDefinition:", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d locations, want 1", len(got))
			}

			path := filepath.Join(server.snapshots.root,
				snapshotDirName(server.snapshotContext().identity, os.Getpid()), tt.kind, tt.file)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("ReadFile:", err)
			}
			text := string(content)

			if !strings.Contains(text, tt.wantBody) {
				t.Errorf("snapshot does not contain the verbatim source %q:\n%s", tt.wantBody, text)
			}
			if !strings.Contains(text, `parameter "IN_AMOUNT" nullability is unknown`) {
				t.Errorf("snapshot does not name the blocking feature:\n%s", text)
			}
			// Every line before the body must be a comment, and the body must
			// not have grown a CREATE header.
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "--") || line == "" {
					continue
				}
				if strings.HasPrefix(strings.ToUpper(line), "CREATE ") {
					t.Errorf("snapshot synthesized a declaration: %q", line)
				}
				break
			}
		})
	}
}

func TestInterBaseDefinitionWritesNoFile(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		character int
		ddlErr    error
	}{
		{
			name:      "object not found",
			text:      "execute procedure myproc",
			character: 20,
			ddlErr:    database.ErrObjectNotFound,
		},
		{
			name:      "unsupported DDL with no source",
			text:      "execute procedure nosource",
			character: 22,
			ddlErr:    &unsupportedDDLError{object: "procedure", name: "NOSOURCE", feature: "nullability is unknown"},
		},
		{
			name:      "any other error",
			text:      "execute procedure myproc",
			character: 20,
			ddlErr:    errors.New("connection reset"),
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
				return "", tt.ddlErr
			})

			got, err := server.interBaseDefinition(context.Background(), repo, definitionCatalog(), definitionParamsAt(tt.character), tt.text)
			if err != nil {
				t.Fatalf("interBaseDefinition returned an error: %v — a definition miss is not a failure", err)
			}
			if len(got) != 0 {
				t.Fatalf("got %d locations, want none", len(got))
			}
			if !snapshotRootIsEmpty(t, server.snapshots) {
				t.Error("a file was written; a stale cache entry must never leave a snapshot behind")
			}
		})
	}
}

func TestInterBaseDefinitionWithoutCapabilityReturnsNil(t *testing.T) {
	cases := []struct {
		name    string
		repo    database.DBRepository
		dbCache *database.DBCache
		text    string
	}{
		{
			name:    "repository does not implement DDLRepository",
			repo:    &database.MockDBRepository{},
			dbCache: definitionCatalog(),
			text:    "execute procedure myproc",
		},
		{
			name:    "no repository at all",
			repo:    nil,
			dbCache: definitionCatalog(),
			text:    "execute procedure myproc",
		},
		{
			name: "catalog has not arrived yet",
			repo: newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
				t.Error("ObjectDDL was called without a catalog")
				return "", nil
			}),
			dbCache: &database.DBCache{},
			text:    "execute procedure myproc",
		},
		{
			name: "identifier is not a catalog object",
			repo: newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
				t.Error("ObjectDDL was called for an unknown identifier")
				return "", nil
			}),
			dbCache: definitionCatalog(),
			text:    "select * from nosuchthing",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := newDefinitionServer(t)
			got, err := server.interBaseDefinition(context.Background(), tt.repo, tt.dbCache, definitionParamsAt(20), tt.text)
			if err != nil {
				t.Fatal("interBaseDefinition:", err)
			}
			if len(got) != 0 {
				t.Fatalf("got %d locations, want none", len(got))
			}
			if !snapshotRootIsEmpty(t, server.snapshots) {
				t.Error("a file was written without a resolved target")
			}
		})
	}
}

func TestInterBaseDefinitionWithoutASnapshotStoreDegrades(t *testing.T) {
	server := newDefinitionServer(t)
	server.snapshots = nil
	repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE \"MYPROC\" AS BEGIN END", nil
	})

	got, err := server.interBaseDefinition(context.Background(), repo, definitionCatalog(), definitionParamsAt(20), "execute procedure myproc")
	if err != nil {
		t.Fatal("interBaseDefinition:", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d locations, want none when the store is disabled", len(got))
	}
}

func TestInterBaseDefinitionFallsBackToAliasResolution(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()
	configureInterBaseTestServer(t, tx)
	tx.server.snapshots = newTestSnapshotStore(t)

	input := "SELECT ci.ID FROM city AS ci"
	tx.textDocumentDidOpen(t, testFileURI, input)

	var got lsp.Definition
	if err := tx.conn.Call(tx.ctx, "textDocument/definition", definitionParamsAt(8), &got); err != nil {
		t.Fatal("conn.Call textDocument/definition:", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d locations, want the in-document alias", len(got))
	}
	if got[0].URI != testFileURI {
		t.Errorf("URI = %q, want the document's own URI %q", got[0].URI, testFileURI)
	}
	if !snapshotRootIsEmpty(t, tx.server.snapshots) {
		t.Error("the alias path wrote a snapshot; it must win outright")
	}
}
```

Add `"context"`, `"os"` and `"path/filepath"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestInterBaseDefinition' ./internal/handler/ -v`
Expected: FAIL to compile — `server.interBaseDefinition undefined`.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/handler/interbase_definition.go`, adding `"context"` and `"net/url"` to the imports:

```go
// definitionDDLTimeout bounds the catalog round trip. textDocument/definition
// stays on the inline dispatch path — Plan 1 moves only workspace/executeCommand
// off it — so an unbounded catalog read would freeze the whole server. The
// bound matches the one hover uses for the same reason.
const definitionDDLTimeout = 3 * time.Second

// interBaseDefinition materialises the source of the catalog object under the
// cursor and returns its location. Every miss returns (nil, nil): the user
// asked to navigate, not to be told about the catalog, so nothing here surfaces
// as a request error.
//
// repo and dbCache are parameters rather than reads off the server so this
// method takes stateMu exactly once, for snapshotContext, and never across the
// file write that follows.
func (s *Server) interBaseDefinition(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.DefinitionParams, text string) (lsp.Definition, error) {
	if s.snapshots == nil || repo == nil {
		return nil, nil
	}
	ddlRepo, ok := repo.(database.DDLRepository)
	if !ok {
		return nil, nil
	}
	target, ok := resolveSnapshotTarget(text, params, dbCache, s.parserDriver())
	if !ok {
		return nil, nil
	}

	ddlCtx, cancel := context.WithTimeout(ctx, definitionDDLTimeout)
	defer cancel()
	ddl, ddlErr := ddlRepo.ObjectDDL(ddlCtx, target.kind, target.name)

	body, note, ok := snapshotBodyFor(ddl, ddlErr, target)
	if !ok {
		return nil, nil
	}

	sc := s.snapshotContext()
	content, bannerLines := renderSnapshot(target, sc, s.snapshots.now(), body, note)
	path, err := s.snapshots.write(sc, string(target.kind), target.name, content)
	if err != nil {
		log.Printf("sqls: write %s snapshot for %q: %v", target.kind, target.name, err)
		return nil, nil
	}

	return []lsp.Location{{
		URI:   snapshotURI(path),
		Range: snapshotRange(content, bannerLines, target.name),
	}}, nil
}

// snapshotURI turns an absolute path into a file:// URI. The path can contain
// percent signs, because escapeSnapshotName puts them there; url.URL.String
// encodes them as %25, so the URI decodes back to the real file name.
func snapshotURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
```

In `internal/handler/definition.go`, replace the body of `handleDefinition` after the unmarshal. **This is the post-Plan-1 shape**: Plan 1 Task 5 already replaced the `s.files[...]` lookup with `s.fileText`, so the lines being replaced read `text, ok := s.fileText(...)` down to the `return definitionWithDriver(...)`:

```go
	text, ok := s.fileText(params.TextDocument.URI)
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	dbCache := s.worker.Cache()
	res, err := definitionWithDriver(params.TextDocument.URI, text, params, dbCache, s.parserDriver())
	if err != nil {
		return nil, err
	}
	// In-document aliases and subqueries win outright, for every driver.
	if len(res) > 0 {
		return res, nil
	}

	// Not having a repository is not a definition failure: for every driver
	// without a catalog, the alias path above is the whole feature.
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, nil
	}
	return s.interBaseDefinition(ctx, repo, dbCache, params, text)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestInterBaseDefinition' ./internal/handler/ -v`
Expected: every test and subtest PASS.

- [ ] **Step 5: Verify the existing definition tests are untouched**

Run: `go test -run 'TestDefinition|Test_definition' ./internal/handler/ -v`
Expected: every case in `definitionTestCases` PASS, unchanged. If any fails, the alias path was altered — revert and re-apply Step 3, which only *appends* a branch after `len(res) > 0`.

- [ ] **Step 6: Verify no snapshot was written outside a temporary directory**

Run: `go test ./internal/handler/ -count=1 && ls "${XDG_CACHE_HOME:-$HOME/.cache}/sqls/interbase-sources" 2>&1`
Expected: the tests pass and the listing reports `No such file or directory` — or, if the directory already existed from a real sqls run, that its contents are unchanged. A directory created by the test run means some test reached `write` through the store `NewServer` built instead of replacing `server.snapshots` with `newTestSnapshotStore(t)`.

- [ ] **Step 7: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/handler/interbase_definition.go internal/handler/interbase_definition_test.go internal/handler/definition.go
git commit -m "feat: go-to-definition opens database-resident source as a snapshot"
```

---

### Task 7: Document the snapshots, including what a crash leaves behind

Spec's Documentation section: the README must cover "that go-to-definition opens a read-only snapshot file under the user cache directory, where that directory is, and that editing it does not change the database", and Risk 3: "a crashed process leaves business logic in the cache directory for up to 24 hours. This is documented in the README rather than hidden."

This is a user-facing security property, so it goes in plain words near the top of the subsection, not in a footnote.

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Find the insertion point**

Run: `grep -n '^### InterBase Build\|^#### InterBase editor features\|^## Editor Plugins' README.md`
Expected: `### InterBase Build` around line 73 and `## Editor Plugins` around line 92. `#### InterBase editor features` exists only if Plan 3 has already landed its README change.

- [ ] **Step 2: Add the documentation**

If `#### InterBase editor features` exists, append the text below inside that subsection. If it does not, insert the heading plus the text after the `### InterBase Build` section's last paragraph (the one ending "does not include this local integration.") and before `## Editor Plugins`.

````markdown
#### InterBase editor features

##### Go-to-definition for database-resident source

Procedures, views and triggers keep their source in the database, not in a file
on disk. `textDocument/definition` therefore materialises that source as a
**read-only snapshot file** and returns an ordinary `file://` location, so any
editor that can open a file can follow the jump — no client-side content
provider and no custom URI scheme.

Snapshots live under the user cache directory, in
`sqls/interbase-sources/<hash>-<pid>/<kind>/<name>.sql`. On Linux that is
`$XDG_CACHE_HOME/sqls/interbase-sources`, or `~/.cache/sqls/interbase-sources`
when `XDG_CACHE_HOME` is unset. There is one directory per connection per server
process; `<hash>` is derived from the connection settings, which are hashed
rather than written so a host name or database path never lands on disk in clear
form. Directories are created with mode `0700` and files with mode `0600`.

**Editing a snapshot does not change the database.** There is no write-back
path. The file is rewritten from the catalog on every jump, so it is never
stale and any local edit is discarded.

**Snapshots contain your database's business logic, and a crash leaves them on
disk.** They are removed when the server shuts down — including when closing the
database connection fails — but a process that dies without shutting down leaves
its directory behind. Such a directory is removed by the next sqls run that uses
the feature, once it is more than 24 hours old. That window is the cost of
portable LSP navigation: there is no way to hand an editor navigable text
without a real file. If it is unacceptable in your environment, do not use
go-to-definition on database objects.

When InterBase's catalog cannot reproduce executable DDL — most commonly because
it does not record whether a procedure parameter is nullable — the snapshot
contains the **verbatim catalog source** under a comment naming what blocked
reproduction. A `CREATE` header is never invented. When the object has
disappeared from the catalog since the connection was cached, no file is written
and the editor reports that no definition was found.

Go-to-definition for in-document aliases and subqueries is unchanged and works
for every driver, with or without a catalog.
````

- [ ] **Step 3: Verify the claims in the text against the code**

Run:

```bash
grep -n 'interbase-sources\|snapshotDirMode\|snapshotFileMode\|snapshotMaxAge' internal/handler/snapshot_store.go
```

Expected: the path fragment is `"sqls", "interbase-sources"`, the modes are `0o700` and `0o600`, and `snapshotMaxAge` is `24 * time.Hour` — matching the README word for word. A documentation change that drifts from the constants is worse than none, because it is the text a security review reads.

- [ ] **Step 4: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document go-to-definition snapshots and their crash exposure"
```
