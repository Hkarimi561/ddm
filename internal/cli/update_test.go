package cli

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.5.0", "v0.5.1", -1},
		{"0.5.1", "v0.5.1", 0},
		{"0.10.0", "v0.9.9", 1},
		{"1.0.0-next", "v1.0.0", 0},
		{"dev", "v0.1.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/o/r/releases/latest" {
			http.Redirect(w, r, "/o/r/releases/tag/v1.2.3", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	old := githubBase
	githubBase = srv.URL
	defer func() { githubBase = old }()

	tag, err := latestTag(t.Context(), srv.Client(), "o/r")
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("latestTag = %q, %v", tag, err)
	}
	if _, err := latestTag(t.Context(), srv.Client(), "o/missing"); err == nil {
		t.Fatal("expected an error for a repository without releases")
	}
}

func TestVerifyChecksumAndExtract(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "ddm_test.zip")
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("ddm.exe")
	w.Write([]byte("binary"))
	zw.Close()
	f.Close()

	b, _ := os.ReadFile(archive)
	sum := sha256.Sum256(b)
	sums := filepath.Join(dir, "checksums.txt")
	os.WriteFile(sums, []byte(hex.EncodeToString(sum[:])+"  ddm_test.zip\n"), 0o644)

	if err := verifyChecksum(archive, sums, "ddm_test.zip"); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksum(archive, sums, "other.zip"); err == nil {
		t.Fatal("expected an error for a missing checksum entry")
	}
	os.WriteFile(sums, []byte(hex.EncodeToString(make([]byte, 32))+"  ddm_test.zip\n"), 0o644)
	if err := verifyChecksum(archive, sums, "ddm_test.zip"); err == nil {
		t.Fatal("expected a checksum mismatch")
	}

	dest := filepath.Join(dir, "out")
	if err := extractBinary(archive, "ddm.exe", dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "binary" {
		t.Fatalf("extracted %q", got)
	}
	if err := extractBinary(archive, "nope", dest); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
