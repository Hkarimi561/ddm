package diskspace

import "testing"

func TestFree(t *testing.T) {
	n, err := Free(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no free space reported for the temp directory")
	}
	if _, err := Free("/definitely/not/a/real/path/ddm"); err == nil {
		t.Error("expected an error for a missing path")
	}
}
