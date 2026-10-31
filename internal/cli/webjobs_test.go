package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{"ps -a", []string{"ps", "-a"}, false},
		{"  run   -d  nginx ", []string{"run", "-d", "nginx"}, false},
		{`run --name "my app" -v 'C:\data:/d' nginx`, []string{"run", "--name", "my app", "-v", `C:\data:/d`, "nginx"}, false},
		{`exec c sh -c "echo \"hi\" && ls"`, []string{"exec", "c", "sh", "-c", `echo "hi" && ls`}, false},
		{`run -v "C:\data":/d x`, []string{"run", "-v", `C:\data:/d`, "x"}, false},
		{`ps; rm -rf / | cat $(id)`, []string{"ps;", "rm", "-rf", "/", "|", "cat", "$(id)"}, false},
		{`run ""`, []string{"run", ""}, false},
		{`run "oops`, nil, true},
		{"", nil, false},
	}
	for _, c := range cases {
		got, err := splitCommand(c.in)
		if (err != nil) != c.bad {
			t.Errorf("splitCommand(%q) error = %v", c.in, err)
			continue
		}
		if !c.bad && !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("splitCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWebCommandArgs(t *testing.T) {
	cases := []struct {
		kind, in string
		want     []string
		bad      bool
	}{
		{"pull", "nginx:latest", []string{"nginx:latest"}, false},
		{"pull", "docker pull nginx", []string{"nginx"}, false},
		{"pull", "ddm pull --platform linux/arm64 x", []string{"--platform", "linux/arm64", "x"}, false},
		{"docker", "ps -a", []string{"ps", "-a"}, false},
		{"docker", "docker run -d nginx", []string{"run", "-d", "nginx"}, false},
		{"docker", "ddm docker ps", []string{"ps"}, false},
		{"docker", "   ", nil, true},
		{"docker", "docker", nil, true},
		{"shell", "ls", nil, true},
	}
	for _, c := range cases {
		got, err := webCommandArgs(c.kind, c.in)
		if (err != nil) != c.bad {
			t.Errorf("webCommandArgs(%q, %q) error = %v", c.kind, c.in, err)
			continue
		}
		if !c.bad && !reflect.DeepEqual(got, c.want) {
			t.Errorf("webCommandArgs(%q, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestJobOutputWindow(t *testing.T) {
	j := &job{status: "running"}
	j.Write([]byte("hello "))
	j.Write([]byte("world"))
	s := j.snapshot(0, true)
	if s.Output != "hello world" || s.Next != 11 {
		t.Fatalf("snapshot = %+v", s)
	}
	if s := j.snapshot(6, true); s.Output != "world" {
		t.Fatalf("from 6: %q", s.Output)
	}
	if s := j.snapshot(11, true); s.Output != "" || s.Next != 11 {
		t.Fatalf("from end: %+v", s)
	}

	// Old output is dropped past the cap, and offsets stay absolute.
	j.Write([]byte(strings.Repeat("x", maxJobOutput)))
	s = j.snapshot(0, true)
	if len(s.Output) != maxJobOutput || s.Next != 11+maxJobOutput {
		t.Fatalf("after overflow: len=%d next=%d", len(s.Output), s.Next)
	}
}
