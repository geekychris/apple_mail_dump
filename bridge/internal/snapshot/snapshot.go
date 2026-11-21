// Package snapshot keeps a private, consistent copy of a live SQLite database
// (Mail's Envelope Index, Messages' chat.db) so the bridge never opens the
// original file through SQLite at all: it only reads the main file and its
// -wal sibling. Mail and Messages are therefore never locked, checkpointed or
// otherwise modified by the bridge.
//
// On APFS the copy is a copy-on-write clone (clonefile), which is instant and
// writes almost nothing even for a multi-GB Envelope Index; elsewhere (another
// file system, another volume, another OS) it falls back to copying bytes.
//
// The copy is refreshed lazily: at most once per MaxAge, and only when the
// source's size or mtime changed.
package snapshot

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0
)

// ErrNoAccess means the source exists (or its directory does) but the
// process may not read it. On macOS this is almost always missing Full Disk
// Access for the bridge binary.
var ErrNoAccess = errors.New("permission denied reading the database (grant Full Disk Access to the bridge binary)")

// ErrNotFound means there is no database at the configured path.
var ErrNotFound = errors.New("database not found")

// Snapshot owns one source database and its private copy.
type Snapshot struct {
	Source string        // live database path
	Dir    string        // private 0700 directory holding the copy
	MaxAge time.Duration // minimum time between freshness checks

	mu        sync.Mutex
	method    string // "clone" or "copy": how the current copy was made
	db        *sql.DB
	gen       int
	checked   time.Time
	sig       string
	refreshed time.Time
}

// New returns a Snapshot of source kept under dir.
func New(source, dir string, maxAge time.Duration) *Snapshot {
	return &Snapshot{Source: source, Dir: dir, MaxAge: maxAge}
}

// DB returns a read-only handle on a recent copy of the database, refreshing
// the copy when it is older than MaxAge and the source has changed.
func (s *Snapshot) DB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil && time.Since(s.checked) < s.MaxAge {
		return s.db, nil
	}
	sig, err := signature(s.Source)
	if err != nil {
		if s.db != nil {
			// Keep serving the last good copy; surface nothing to callers.
			s.checked = time.Now()
			return s.db, nil
		}
		return nil, err
	}
	s.checked = time.Now()
	if s.db != nil && sig == s.sig {
		return s.db, nil
	}
	db, err := s.refresh()
	if err != nil {
		if s.db != nil {
			return s.db, nil
		}
		return nil, err
	}
	if s.db != nil {
		// Old handle: in-flight queries on it hold their own connection; close
		// lets them finish (database/sql waits for released conns).
		old := s.db
		go func() { _ = old.Close() }()
	}
	s.db, s.sig, s.refreshed = db, sig, time.Now()
	return db, nil
}

// Method says how the current copy was made: "clone" (APFS copy-on-write)
// or "copy" (bytes). Empty before the first copy.
func (s *Snapshot) Method() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.method
}

// RefreshedAt is when the current copy was taken (zero if never).
func (s *Snapshot) RefreshedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshed
}

// Close releases the copy.
func (s *Snapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Snapshot) refresh() (*sql.DB, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	s.gen++
	dst := filepath.Join(s.Dir, fmt.Sprintf("%s.%d", filepath.Base(s.Source), s.gen))
	// Copy main file then WAL; if either changed while copying, try again so
	// the pair is consistent (a checkpoint between the two copies would
	// otherwise give a torn view).
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		before, err := signature(s.Source)
		if err != nil {
			return nil, err
		}
		method, err := copyPair(s.Source, dst)
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrNoAccess) || errors.Is(err, ErrNotFound) {
				return nil, err
			}
			continue
		}
		after, err := signature(s.Source)
		if err != nil {
			return nil, err
		}
		if before != after {
			lastErr = errors.New("source changed during copy")
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}
		db, err := openCopy(dst)
		if err != nil {
			lastErr = err
			continue
		}
		cleanupOld(s.Dir, filepath.Base(s.Source), dst)
		s.method = method
		return db, nil
	}
	return nil, fmt.Errorf("snapshot %s: %w", filepath.Base(s.Source), lastErr)
}

// openCopy opens our private copy. It is opened read-write only so SQLite can
// fold the copied WAL into it (a mode=ro open of a WAL database without a
// writable -shm fails); the copy is ours alone and nothing writes to it after
// that. query_only stops any statement from modifying it.
func openCopy(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// Force WAL recovery now so later readers see the folded state.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// cloner is swapped in tests to simulate a file system without clonefile.
var cloner = cloneFile

// copyPair copies the main file and, if present, the -wal. It reports
// "clone" when both were APFS clones and "copy" when any was copied byte by
// byte.
func copyPair(src, dst string) (string, error) {
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dst + suf)
	}
	method := "clone"
	m, err := cloneOrCopy(src, dst)
	if err != nil {
		return "", err
	}
	if m == "copy" {
		method = "copy"
	}
	if _, err := os.Stat(src + "-wal"); err == nil {
		m, err := cloneOrCopy(src+"-wal", dst+"-wal")
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if m == "copy" {
			method = "copy"
		}
	}
	return method, nil
}

// cloneOrCopy clones src to dst, or copies its bytes when cloning fails. A
// clone keeps src's mode, so it is set to 0600 afterwards like a copy.
func cloneOrCopy(src, dst string) (string, error) {
	if err := cloner(src, dst); err == nil {
		if err := os.Chmod(dst, 0o600); err != nil {
			return "", err
		}
		return "clone", nil
	} else if errors.Is(classify(err), ErrNoAccess) || errors.Is(classify(err), ErrNotFound) {
		return "", classify(err)
	}
	_ = os.Remove(dst) // a failed clone may leave nothing or a partial entry
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return "copy", nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return classify(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return classify(err)
	}
	return out.Close()
}

func signature(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", classify(err)
	}
	sig := fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	if w, err := os.Stat(path + "-wal"); err == nil {
		sig += fmt.Sprintf("|%d:%d", w.Size(), w.ModTime().UnixNano())
	}
	return sig, nil
}

func classify(err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		return ErrNoAccess
	case errors.Is(err, os.ErrNotExist):
		return ErrNotFound
	}
	// macOS TCC denials surface as EPERM ("operation not permitted").
	var pe *os.PathError
	if errors.As(err, &pe) && pe.Err != nil && pe.Err.Error() == "operation not permitted" {
		return ErrNoAccess
	}
	return err
}

func cleanupOld(dir, base, keep string) {
	matches, _ := filepath.Glob(filepath.Join(dir, base+".*"))
	for _, m := range matches {
		if m == keep || m == keep+"-wal" || m == keep+"-shm" {
			continue
		}
		// Older generations may still have a draining handle; removing the
		// directory entry is safe on Unix (open fds keep the inode).
		_ = os.Remove(m)
	}
}
