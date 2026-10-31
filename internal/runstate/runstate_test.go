package runstate

import (
	"os"
	"testing"
	"time"

	"github.com/Hkarimi561/ddm/internal/engine"
)

func TestRunPublishesProgressAndObeysPause(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gate := engine.NewGate()
	var notified []bool
	run, err := d.Start("pull nginx", gate, engine.NewLimiter(1<<20), func(p bool) { notified = append(notified, p) })
	if err != nil {
		t.Fatal(err)
	}

	run.Start(engine.Blob{Digest: "sha256:aa", Size: 1000, Name: "layer 1/1"}, 100)
	run.Add("sha256:aa", 400)
	run.Source("sha256:aa", "https://mirror.example.com")

	var s Status
	waitFor(t, func() bool {
		runs, err := d.List()
		if err != nil || len(runs) != 1 {
			return false
		}
		s = runs[0]
		return s.Done == 500
	}, "the status file to show the progress")
	if s.PID != os.Getpid() || s.Command != "pull nginx" || s.Done != 500 || s.Total != 1000 || s.RateLimit != 1<<20 {
		t.Errorf("status = %+v", s)
	}
	if len(s.Items) != 1 || s.Items[0].Source != "https://mirror.example.com" || s.Items[0].Done != 500 {
		t.Errorf("items = %+v", s.Items)
	}

	if pids, err := d.Pause(0); err != nil || len(pids) != 1 {
		t.Fatalf("Pause = %v, %v", pids, err)
	}
	waitFor(t, gate.Paused, "the gate to close after `ddm pause`")
	if pids, err := d.Resume(0); err != nil || len(pids) != 1 {
		t.Fatalf("Resume = %v, %v", pids, err)
	}
	waitFor(t, func() bool { return !gate.Paused() }, "the gate to open after `ddm resume`")

	if _, err := d.Pause(999999); err == nil {
		t.Error("pausing an unknown PID succeeded")
	}

	if pids, err := d.SetLimit(os.Getpid(), 512<<10); err != nil || len(pids) != 1 {
		t.Fatalf("SetLimit = %v, %v", pids, err)
	}
	waitFor(t, func() bool { return run.limiter.Rate() == 512<<10 }, "the new bandwidth limit to apply")

	run.Close()
	if runs, _ := d.List(); len(runs) != 0 {
		t.Errorf("status left behind after Close: %+v", runs)
	}
	if len(notified) != 2 || !notified[0] || notified[1] {
		t.Errorf("pause notifications = %v, want [true false]", notified)
	}
}

func TestListDropsStaleEntries(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := `{"pid": 4242, "command": "pull x", "updated": "2001-01-01T00:00:00Z"}`
	if err := os.WriteFile(d.statusPath(4242), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.pausePath(4242), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if runs, _ := d.List(); len(runs) != 0 {
		t.Fatalf("a dead process is listed: %+v", runs)
	}
	for _, p := range []string{d.statusPath(4242), d.pausePath(4242)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was not cleaned up", p)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
