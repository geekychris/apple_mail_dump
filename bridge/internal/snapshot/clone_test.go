package snapshot

import (
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/geekychris/apple_mail_dump/bridge/internal/fixtures"
)

func rows(t *testing.T, s *Snapshot) int {
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

func mailFixture(t *testing.T) *fixtures.Mail {
	m := fixtures.NewMail(t)
	m.AddMessage(t, 1, 1_700_000_000, "one")
	m.AddMessage(t, 2, 1_700_000_100, "two")
	if _, err := os.Stat(m.Envelope + "-wal"); err != nil {
		t.Fatalf("fixture should keep rows in the WAL: %v", err)
	}
	return m
}

// On APFS (macOS temp directories are) the snapshot is a clone, sees the WAL
// rows, is private (0600) and leaves the source untouched.
func TestSnapshotClonesOnAPFS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clonefile is macOS-only")
	}
	m := mailFixture(t)
	probe := filepath.Join(filepath.Dir(m.Envelope), "probe")
	if err := cloneFile(m.Envelope, probe); err != nil {
		t.Skipf("temp dir is not on APFS: %v", err)
	}
	os.Remove(probe)
	before := sum(t, m.Envelope)

	s := New(m.Envelope, filepath.Join(filepath.Dir(m.Envelope), "snap"), 0)
	defer s.Close()
	if n := rows(t, s); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if s.Method() != "clone" {
		t.Fatalf("method = %q, want clone", s.Method())
	}
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "Envelope Index.*"))
	for _, p := range matches {
		if st, _ := os.Stat(p); st != nil && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o, want 600", filepath.Base(p), st.Mode().Perm())
		}
	}
	if sum(t, m.Envelope) != before {
		t.Fatal("source changed")
	}
	// A clone is independent: new source rows only appear after a refresh.
	m.AddMessage(t, 3, 1_700_000_200, "three")
	if n := rows(t, s); n != 3 {
		t.Fatalf("after refresh rows = %d, want 3", n)
	}
}

// When clonefile is unavailable (a file system without it) the snapshot
// copies bytes and is otherwise identical.
func TestSnapshotFallsBackToCopyWhenCloneFails(t *testing.T) {
	orig := cloner
	defer func() { cloner = orig }()
	calls := 0
	cloner = func(src, dst string) error { calls++; return errors.New("operation not supported") }

	m := mailFixture(t)
	s := New(m.Envelope, t.TempDir(), 0)
	defer s.Close()
	if n := rows(t, s); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if s.Method() != "copy" || calls == 0 {
		t.Fatalf("method = %q after %d clone attempts, want copy", s.Method(), calls)
	}
}

// A real non-APFS volume: a small FAT disk image attached with hdiutil (no
// root needed). The source lives on it and the snapshot on APFS, so
// clonefile fails (EXDEV / ENOTSUP) and the byte copy takes over.
func TestSnapshotFallsBackToCopyOnNonAPFSVolume(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hdiutil is macOS-only")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("no hdiutil")
	}
	tmp := t.TempDir()
	img := filepath.Join(tmp, "fat.dmg")
	mnt := filepath.Join(tmp, "mnt")
	if out, err := exec.Command("hdiutil", "create", "-size", "8m", "-fs", "MS-DOS", "-volname", "MBTEST", img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create: %v %s", err, out)
	}
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-noverify", "-mountpoint", mnt, img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil attach: %v %s", err, out)
	}
	defer exec.Command("hdiutil", "detach", "-force", mnt).Run()

	m := mailFixture(t)
	src := filepath.Join(mnt, "Envelope Index")
	for _, suf := range []string{"", "-wal"} {
		b, err := os.ReadFile(m.Envelope + suf)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src+suf, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cloneFile(src, filepath.Join(tmp, "probe")); err == nil {
		t.Fatal("clonefile unexpectedly worked from a FAT volume")
	}
	s := New(src, filepath.Join(tmp, "snap"), 0)
	defer s.Close()
	if n := rows(t, s); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if s.Method() != "copy" {
		t.Fatalf("method = %q, want copy", s.Method())
	}
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}
