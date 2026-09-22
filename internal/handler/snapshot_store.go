package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	snapshotDirMode  os.FileMode = 0o700
	snapshotFileMode os.FileMode = 0o600
)

// snapshotMaxAge bounds how long a crashed process's snapshots survive. It is
// the only thing bounding them: a process that dies without running Stop leaves
// database source on disk until some later sqls run prunes it.
const snapshotMaxAge = 24 * time.Hour

// snapshotDirPattern matches the <hash>-<pid> directories this store owns.
// Pruning removes nothing else: the root is a directory in the user's cache,
// and an entry that does not match this shape is not ours to delete.
var snapshotDirPattern = regexp.MustCompile(`^[0-9a-f]{16}-[0-9]+$`)

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

	s.pruneLocked()

	dir, err := s.connectionDirLocked(sc)
	if err != nil {
		return "", err
	}
	// kind is escaped too. Every caller passes an ObjectKind constant today, so
	// nothing unsafe is reachable — but the parameter is a plain string so this
	// file stays free of the database package, which removes the type-level
	// guarantee. One call keeps it closed permanently.
	kindDir := filepath.Join(dir, escapeSnapshotName(kind))
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

	// A file created or overwritten inside kindDir bumps kindDir's own mtime,
	// never dir's: a directory's mtime moves only when an entry is added to or
	// removed from it directly, and dir gains such an entry only the rare time
	// a kind is written for the first time. Without this, a long-lived
	// connection's directory can sit past pruneLocked's 24-hour cutoff while
	// still in active use. Touch it on every write so it cannot. Best effort:
	// a failure here must not discard a snapshot that was otherwise written
	// successfully.
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		log.Printf("sqls: touch snapshot connection directory %q: %v", dir, err)
	}

	return path, nil
}

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
//
// The escape is injective — `%` itself becomes `%25` — so two distinct catalog
// names can never collide on one file. The result is bounded at three bytes per
// input byte, so an all-escaped 31-byte Dialect 1 name yields 93 bytes and 97
// with the `.sql` suffix, well inside every filesystem's 255-byte limit.
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
