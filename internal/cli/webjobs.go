package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Commands started from the web UI run as child `ddm pull` / `ddm docker`
// processes, so they behave exactly like the same command in a terminal
// (resumable downloads, mirrors, `ddm status`), and a crash or Ctrl+C
// handling bug in one can't take the web UI down with it.

const (
	maxJobs        = 30      // finished jobs kept in memory
	maxRunningJobs = 4       // commands running at the same time
	maxJobOutput   = 2 << 20 // bytes of output kept per job
)

type job struct {
	ID      int
	Kind    string // "pull" or "docker"
	Command string // what the user typed
	Started time.Time

	cancel context.CancelFunc

	mu       sync.Mutex
	buf      []byte
	base     int    // absolute offset of buf[0]; grows when old output is dropped
	status   string // running, done, failed, cancelled
	exit     int
	ended    time.Time
	canceled bool
}

// Write implements io.Writer for the child's stdout and stderr.
func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, p...)
	if over := len(j.buf) - maxJobOutput; over > 0 {
		j.buf = append([]byte(nil), j.buf[over:]...)
		j.base += over
	}
	return len(p), nil
}

type jobJSON struct {
	ID      int    `json:"id"`
	Kind    string `json:"kind"`
	Command string `json:"command"`
	Started string `json:"started"`
	Ended   string `json:"ended,omitempty"`
	Status  string `json:"status"`
	Exit    int    `json:"exit"`
	// Output is the part of the output from offset `from`; Next is where the
	// caller should continue.
	Output string `json:"output,omitempty"`
	Next   int    `json:"next"`
}

func (j *job) snapshot(from int, withOutput bool) jobJSON {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := jobJSON{
		ID: j.ID, Kind: j.Kind, Command: j.Command,
		Started: j.Started.Format(time.RFC3339), Status: j.status, Exit: j.exit,
		Next: j.base + len(j.buf),
	}
	if !j.ended.IsZero() {
		out.Ended = j.ended.Format(time.RFC3339)
	}
	if withOutput {
		if from < j.base {
			from = j.base
		}
		if from < out.Next {
			out.Output = strings.ToValidUTF8(string(j.buf[from-j.base:]), "�")
		}
	}
	return out
}

type jobs struct {
	mu   sync.Mutex
	next int
	list []*job // oldest first
}

func (js *jobs) get(id int) *job {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.list {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (j *job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == "running"
}

// start launches ddm with args and returns the job.
func (js *jobs) start(g *globals, kind, command string, args []string) (*job, error) {
	exe, err := executable()
	if err != nil {
		return nil, err
	}
	js.mu.Lock()
	running := 0
	for _, j := range js.list {
		if j.running() {
			running++
		}
	}
	if running >= maxRunningJobs {
		js.mu.Unlock()
		return nil, fmt.Errorf("%d commands are already running; cancel or wait for one", running)
	}
	js.next++
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{ID: js.next, Kind: kind, Command: command, Started: time.Now(), cancel: cancel, status: "running"}
	js.list = append(js.list, j)
	// Forget the oldest finished jobs.
	for len(js.list) > maxJobs {
		dropped := false
		for i, o := range js.list {
			if !o.running() {
				js.list = append(js.list[:i], js.list[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			break
		}
	}
	js.mu.Unlock()

	var full []string
	if g.configPath != "" {
		full = append(full, "--config", g.configPath)
	}
	if g.cacheDir != "" {
		full = append(full, "--cache-dir", g.cacheDir)
	}
	full = append(full, kind)
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, exe, full...)
	cmd.Stdout, cmd.Stderr = j, j
	cmd.Env = append(cmdEnv(), "NO_COLOR=1")
	prepareChild(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	if err := cmd.Start(); err != nil {
		j.finish(err, ctx)
		return j, nil
	}
	go func() {
		err := cmd.Wait()
		j.finish(err, ctx)
		cancel()
	}()
	return j, nil
}

func (j *job) finish(err error, ctx context.Context) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ended = time.Now()
	var ee *exec.ExitError
	switch {
	case j.canceled || ctx.Err() != nil:
		j.status = "cancelled"
	case err == nil:
		j.status = "done"
	case errors.As(err, &ee):
		j.status, j.exit = "failed", ee.ExitCode()
	default:
		j.status, j.exit = "failed", -1
		j.buf = append(j.buf, []byte("ddm: "+err.Error()+"\n")...)
	}
}

// splitCommand splits a command line into arguments the way a shell would
// for simple cases: spaces separate arguments, and single or double quotes
// group words. Nothing is expanded and no shell is involved, so ";", "&&",
// "|" and "$(...)" are ordinary characters passed to docker as they are.
// Inside double quotes only \" and \\ are escapes, so Windows paths work.
func splitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote == '"' && c == '\\' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\'):
			i++
			cur.WriteRune(rs[i])
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			quote, inArg = c, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(c)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in the command")
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

// webCommandArgs turns what was typed in the web UI into ddm arguments.
// A leading "docker", "ddm", "pull" is accepted and dropped, so pasting a
// whole command line works.
func webCommandArgs(kind, command string) ([]string, error) {
	args, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	drop := func(words ...string) {
		for len(args) > 0 {
			found := false
			for _, w := range words {
				if strings.EqualFold(args[0], w) {
					args, found = args[1:], true
					break
				}
			}
			if !found {
				return
			}
		}
	}
	switch kind {
	case "pull":
		// "ddm pull x", "docker pull x" and "x" all mean the same here.
		drop("ddm", "docker")
		drop("pull")
	case "docker":
		drop("ddm")
		drop("docker")
	default:
		return nil, fmt.Errorf("unknown kind %q (want pull or docker)", kind)
	}
	if len(args) == 0 {
		return nil, errors.New("type a command first")
	}
	return args, nil
}

func (s *uiServer) commandsEnabled() bool {
	f, err := s.configFile()
	return err != nil || f.Config.UICommands
}

func (s *uiServer) listJobs(w http.ResponseWriter, _ *http.Request) {
	s.jobs.mu.Lock()
	list := append([]*job(nil), s.jobs.list...)
	s.jobs.mu.Unlock()
	out := make([]jobJSON, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- { // newest first
		out = append(out, list[i].snapshot(0, false))
	}
	writeJSON(w, map[string]any{"enabled": s.commandsEnabled(), "jobs": out})
}

func (s *uiServer) startJob(w http.ResponseWriter, r *http.Request) {
	if !s.commandsEnabled() {
		writeJSONError(w, http.StatusForbidden, errors.New("running commands from the web UI is turned off (ddm config set ui_commands true)"))
		return
	}
	var req struct{ Kind, Command string }
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	args, err := webCommandArgs(req.Kind, req.Command)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	j, err := s.jobs.start(s.g, req.Kind, strings.TrimSpace(req.Command), args)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, j.snapshot(0, true))
}

func (s *uiServer) jobFromPath(w http.ResponseWriter, r *http.Request) *job {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, errors.New("bad job id"))
		return nil
	}
	j := s.jobs.get(id)
	if j == nil {
		writeJSONError(w, http.StatusNotFound, errors.New("no such command (it may have been forgotten)"))
	}
	return j
}

func (s *uiServer) getJob(w http.ResponseWriter, r *http.Request) {
	j := s.jobFromPath(w, r)
	if j == nil {
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	writeJSON(w, j.snapshot(from, true))
}

func (s *uiServer) cancelJob(w http.ResponseWriter, r *http.Request) {
	j := s.jobFromPath(w, r)
	if j == nil {
		return
	}
	j.mu.Lock()
	j.canceled = j.status == "running"
	j.mu.Unlock()
	j.cancel()
	writeJSON(w, map[string]bool{"ok": true})
}
