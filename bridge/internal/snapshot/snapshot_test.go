package snapshot_test

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
	"github.com/geekychris/apple_mail_dump/bridge/internal/snapshot"
)

func digest(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func count(t *testing.T, s *snapshot.Snapshot) int {
	t.Helper()
	db, err := s.DB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSnapshotSeesWALAndNeverTouchesSource(t *testing.T) {
	m := fixtures.NewMail(t)
	m.AddMessage(t, 1, 1_700_000_000, "one")
	m.AddMessage(t, 2, 1_700_000_100, "two")
	if _, err := os.Stat(m.Envelope + "-wal"); err != nil {
		t.Fatalf("fixture should leave rows in the WAL: %v", err)
	}
	mainBefore, walBefore := digest(t, m.Envelope), digest(t, m.Envelope+"-wal")

	s := snapshot.New(m.Envelope, filepath.Join(t.TempDir(), "snap"), 0)
	defer s.Close()
	if n := count(t, s); n != 2 {
		t.Fatalf("snapshot rows = %d, want 2 (rows only in the WAL must be visible)", n)
	}
	if digest(t, m.Envelope) != mainBefore || digest(t, m.Envelope+"-wal") != walBefore {
		t.Fatal("source database or WAL changed after snapshotting")
	}

	// A write to the snapshot is refused (query_only).
	db, _ := s.DB()
	if _, err := db.Exec(`DELETE FROM messages`); err == nil {
		t.Fatal("snapshot accepted a write")
	}

	// New source rows appear after the next refresh.
	m.AddMessage(t, 3, 1_700_000_200, "three")
	time.Sleep(10 * time.Millisecond)
	if n := count(t, s); n != 3 {
		t.Fatalf("after refresh rows = %d, want 3", n)
	}
	st, _ := os.Stat(filepath.Dir(filepath.Join(s.Dir, "x")))
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot dir mode %o, want 700", st.Mode().Perm())
	}
}

func TestSnapshotMaxAgeCachesHandle(t *testing.T) {
	m := fixtures.NewMail(t)
	m.AddMessage(t, 1, 1_700_000_000, "one")
	s := snapshot.New(m.Envelope, t.TempDir(), time.Hour)
	defer s.Close()
	if n := count(t, s); n != 1 {
		t.Fatal(n)
	}
	m.AddMessage(t, 2, 1_700_000_100, "two")
	if n := count(t, s); n != 1 {
		t.Fatalf("within MaxAge the old copy should be served, got %d rows", n)
	}
}

func TestSnapshotErrors(t *testing.T) {
	s := snapshot.New(filepath.Join(t.TempDir(), "missing.db"), t.TempDir(), 0)
	if _, err := s.DB(); !errors.Is(err, snapshot.ErrNotFound) {
		t.Fatalf("missing file: got %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores file modes")
	}
	m := fixtures.NewMail(t)
	m.AddMessage(t, 1, 1_700_000_000, "one")
	if err := os.Chmod(m.Envelope, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(m.Envelope, 0o600)
	s = snapshot.New(m.Envelope, t.TempDir(), 0)
	if _, err := s.DB(); !errors.Is(err, snapshot.ErrNoAccess) {
		t.Fatalf("unreadable file: got %v", err)
	}
}
