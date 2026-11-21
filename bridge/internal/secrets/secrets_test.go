package secrets

import (
	"os"
	"testing"
)

func TestEnsureCreatesPrivateFilesOnce(t *testing.T) {
	f := Files{Dir: t.TempDir() + "/state"}
	created, err := f.Ensure([]string{"m1.local", "192.168.1.70"})
	if err != nil || len(created) != 2 {
		t.Fatalf("%v %v", created, err)
	}
	for _, p := range []string{f.Token(), f.Key(), f.Cert()} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v %v", p, st.Mode().Perm(), err)
		}
	}
	tok1, err := f.LoadToken()
	if err != nil || len(tok1) < 40 {
		t.Fatalf("token: len %d %v", len(tok1), err)
	}
	_, fp1, _ := f.LoadCert()
	again, _ := f.Ensure(nil)
	tok2, _ := f.LoadToken()
	_, fp2, _ := f.LoadCert()
	if len(again) != 0 || tok1 != tok2 || fp1 != fp2 {
		t.Fatal("Ensure must not regenerate existing material")
	}
	os.Chmod(f.Token(), 0o644)
	if _, err := f.LoadToken(); err == nil {
		t.Fatal("world-readable token accepted")
	}
}

func TestTokenEqual(t *testing.T) {
	if !TokenEqual("abc", "abc") || TokenEqual("abc", "abd") || TokenEqual("", "") || TokenEqual("abc", "") {
		t.Fatal("TokenEqual wrong")
	}
}
