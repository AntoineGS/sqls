package handler

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
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

	// Every directory this store creates must be 0700: the kind directory and
	// the per-connection directory. The root is deliberately NOT checked — the
	// store never creates it here (t.TempDir does, at its own mode), so
	// asserting on it would pin something this code does not control.
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

func TestSnapshotStoreCannotEscapeItsDirectoryWithBareDotDot(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	// A name of exactly ".." has no separator for escapeSnapshotName to catch —
	// "." is kept unescaped. Containment here comes from the ".sql" suffix
	// appended after escaping: the resulting file name is "...sql", a plain
	// (if odd-looking) file, never the two-dot path component a filesystem
	// treats specially.
	path, err := store.write(sc, "procedure", "..", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}
	kindDir := filepath.Join(store.root, snapshotDirName(sc.identity, os.Getpid()), "procedure")
	if got := filepath.Dir(path); got != kindDir {
		t.Fatalf("bare .. escaped the kind directory: wrote to %q, want a file directly in %q", path, kindDir)
	}
	if got, want := filepath.Base(path), "...sql"; got != want {
		t.Errorf("snapshot file for name %q = %q, want %q", "..", got, want)
	}
}

func TestEscapeSnapshotNameIsInjective(t *testing.T) {
	// A scheme that escaped "/" but not "%" would make these two distinct
	// catalog names collide: the second is a literal string that looks exactly
	// like the first's escaped form. escapeSnapshotName escapes "%" itself
	// (to "%25"), so no encoded output is also a valid input's literal spelling.
	pairs := [][2]string{
		{"a/b", "a%2Fb"},
		{"MY$PROC", "MY%24PROC"},
	}
	for _, p := range pairs {
		a, b := escapeSnapshotName(p[0]), escapeSnapshotName(p[1])
		if a == b {
			t.Errorf("escapeSnapshotName(%q) and escapeSnapshotName(%q) collided on %q", p[0], p[1], a)
		}
	}
}

func TestSnapshotStoreDistinctNamesNeverCollideOnDisk(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	firstPath, err := store.write(sc, "procedure", "a/b", "FIRST\n")
	if err != nil {
		t.Fatal("first write:", err)
	}
	secondPath, err := store.write(sc, "procedure", "a%2Fb", "SECOND\n")
	if err != nil {
		t.Fatal("second write:", err)
	}

	if firstPath == secondPath {
		t.Fatalf("distinct catalog names %q and %q collided on one file %q", "a/b", "a%2Fb", firstPath)
	}
	firstContent, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal("ReadFile first:", err)
	}
	secondContent, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal("ReadFile second:", err)
	}
	if got, want := string(firstContent), "FIRST\n"; got != want {
		t.Errorf("first snapshot content = %q, want %q — a colliding name overwrote it", got, want)
	}
	if got, want := string(secondContent), "SECOND\n"; got != want {
		t.Errorf("second snapshot content = %q, want %q", got, want)
	}
}

func TestEscapeSnapshotNameHandlesEmptyAndLongNames(t *testing.T) {
	if got := escapeSnapshotName(""); got != "" {
		t.Errorf("escapeSnapshotName(\"\") = %q, want empty string", got)
	}
	if got, want := escapeSnapshotName(".hidden"), ".hidden"; got != want {
		t.Errorf("escapeSnapshotName(%q) = %q, want %q", ".hidden", got, want)
	}

	long := strings.Repeat("$", 100)
	escaped := escapeSnapshotName(long)
	if want := strings.Repeat("%24", 100); escaped != want {
		t.Errorf("escapeSnapshotName(100 dollars) = %q, want %q", escaped, want)
	}
	if len(escaped) != 300 {
		t.Errorf("escaped length = %d, want %d (3 bytes per escaped byte)", len(escaped), 300)
	}
}

func TestSnapshotStoreEmptyNameProducesADefinedFile(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	path, err := store.write(sc, "procedure", "", "BEGIN END\n")
	if err != nil {
		t.Fatal("write:", err)
	}
	if got, want := filepath.Base(path), ".sql"; got != want {
		t.Errorf("empty-name snapshot file = %q, want %q", got, want)
	}
}

func TestSnapshotStoreFailsCleanlyWhenNameExceedsFilesystemLimit(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	// Every escaped byte triples in length, so 100 bytes of "$" yield a
	// 300-byte file name component, past the 255-byte NAME_MAX every Linux
	// filesystem enforces (t.TempDir here sits under /tmp). write must return
	// an error, not truncate the name, panic, or let it collide with a
	// shorter one.
	overLong := strings.Repeat("$", 100)
	if _, err := store.write(sc, "procedure", overLong, "BEGIN END\n"); err == nil {
		t.Fatal("write with an over-long escaped name unexpectedly succeeded")
	}
}

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

// TestSnapshotStoreWriteRefreshesTheConnectionDirectoryMTime proves the actual
// liveness protection: a connection directory that is still being written to
// must never look stale to pruneLocked's mtime cutoff, no matter how long ago
// the directory itself was created.
//
// The second write deliberately reuses the same kind and name as the first.
// Creating a *new* kind subdirectory bumps dir's own mtime as a side effect
// (verified empirically: mkdir always bumps its parent), which would make this
// test pass even without the fix and prove nothing. Overwriting a file inside
// an already-existing kind directory does not — dir's mtime only ever moves
// today when write explicitly touches it — so this is the one path that
// isolates the mechanism this finding is about.
//
// A full backdate-write-prune-survives cycle using a second, independently
// pruning store is not attempted here: any directory this test can create
// necessarily carries this test process's own real pid (connectionDirLocked
// always calls os.Getpid()), so any prune pass run from within this same
// process would exempt it via pruneLocked's pid self-check regardless of
// mtime, making that broader shape of test pass unconditionally and prove
// nothing about the mtime mechanism specifically. Asserting directly against
// pruneLocked's own cutoff expression is the precise, unconfounded proof that
// the mtime it will see is fresh.
func TestSnapshotStoreWriteRefreshesTheConnectionDirectoryMTime(t *testing.T) {
	store := newTestSnapshotStore(t)
	sc := testSnapshotContext()

	if _, err := store.write(sc, "procedure", "MYPROC", "BEGIN END\n"); err != nil {
		t.Fatal("first write:", err)
	}

	dir := filepath.Join(store.root, snapshotDirName(sc.identity, os.Getpid()))
	backdate(t, dir)

	if _, err := store.write(sc, "procedure", "MYPROC", "BEGIN END updated\n"); err != nil {
		t.Fatal("second write:", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal("Stat connection directory:", err)
	}
	if cutoff := time.Now().Add(-snapshotMaxAge); info.ModTime().Before(cutoff) {
		t.Errorf("connection directory mtime = %v, still before pruneLocked's %v cutoff after a write through it — a live connection's directory can cross the staleness cutoff while still in active use", info.ModTime(), cutoff)
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
	select {
	case <-server.cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup worker did not finish")
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

	// Stop schedules cleanup; native Close errors are logged and do not delay
	// the protocol response. Await the private cleanup fence before inspecting.
	if err := server.Stop(); err != nil {
		t.Fatalf("Stop() scheduling error = %v", err)
	}
	select {
	case <-server.cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup worker did not finish")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("snapshot survived a failing shutdown (err=%v)", err)
	}
}

func TestSnapshotContextTracksTheConnectionGeneration(t *testing.T) {
	server := NewServer()
	defer server.Stop()

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
	defer server.Stop()

	sc := server.snapshotContext()
	if sc.label == "" {
		t.Error("label is empty; the banner must always name something")
	}
}
