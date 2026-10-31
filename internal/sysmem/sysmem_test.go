package sysmem

import "testing"

func TestTotal(t *testing.T) {
	n, err := Total()
	if err != nil {
		t.Fatal(err)
	}
	if n < 128<<20 {
		t.Fatalf("implausible total memory: %d bytes", n)
	}
}
