package uiauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	path := PasswordPath(filepath.Join(t.TempDir(), "ddm", "config.yaml"))
	if Exists(path) {
		t.Fatal("password exists before being set")
	}
	if err := Set(path, "short"); err == nil {
		t.Fatal("a too-short password was accepted")
	}
	if err := Set(path, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if ok, err := Check(path, "correct horse battery"); !ok || err != nil {
		t.Fatalf("right password rejected: %v %v", ok, err)
	}
	if ok, err := Check(path, "wrong password"); ok || err != nil {
		t.Fatalf("wrong password accepted: %v %v", ok, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("password file mode %v, want 0600", fi.Mode().Perm())
		}
	}
	b, _ := os.ReadFile(path)
	if string(b) == "correct horse battery" {
		t.Fatal("the password is stored in plain text")
	}
}

func TestGenerate(t *testing.T) {
	a, b := Generate(), Generate()
	if a == b || len(a) != 19 || a[4] != '-' {
		t.Errorf("Generate gave %q and %q", a, b)
	}
}

func TestSessions(t *testing.T) {
	s := NewSessions(time.Hour)
	tok := s.Create()
	if !s.Valid(tok) || s.Valid("nope") || s.Valid("") {
		t.Fatal("session validation is wrong")
	}
	s.Revoke(tok)
	if s.Valid(tok) {
		t.Fatal("revoked session still valid")
	}
	short := NewSessions(time.Millisecond)
	tok = short.Create()
	time.Sleep(5 * time.Millisecond)
	if short.Valid(tok) {
		t.Fatal("expired session still valid")
	}
	tok = s.Create()
	s.RevokeAll()
	if s.Valid(tok) {
		t.Fatal("RevokeAll kept a session")
	}
}

func TestThrottle(t *testing.T) {
	th := &Throttle{Max: 3, Window: time.Hour}
	for range 3 {
		if !th.Allowed() {
			t.Fatal("blocked too early")
		}
		th.Fail()
	}
	if th.Allowed() {
		t.Fatal("not blocked after too many failures")
	}
}
