package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/uiauth"
)

//go:embed webui/index.html
var uiPage []byte

const sessionCookie = "ddm_session"

func newServeCmd(g *globals) *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web UI (normally started for you by `ddm service install`)",
		Long: `Runs the web UI: change settings (bandwidth limit, connections, proxy,
...), manage mirrors, watch, pause and throttle downloads, and clean up the
cache. It listens on ui_address (default 127.0.0.1:7878) and asks for the
password set with "ddm service password". "ddm service install" starts it
automatically at login, so you rarely run this yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}
			if address == "" {
				address = cfg.Config.UIAddress
			}
			pwPath := uiauth.PasswordPath(cfg.Path)
			if !uiauth.Exists(pwPath) {
				pw := uiauth.Generate()
				if err := uiauth.Set(pwPath, pw); err != nil {
					return err
				}
				fmt.Printf("Generated a password for the web UI: %s\nChange it in the UI or with `ddm service password`.\n", pw)
			}
			ln, err := net.Listen("tcp", address)
			if err != nil {
				return fmt.Errorf("starting the web UI on %s: %w (is it already running? see `ddm service status`)", address, err)
			}
			srv := &http.Server{Handler: newUIServer(g, pwPath), ReadHeaderTimeout: 10 * time.Second}
			stopPID := g.writeUIPID()
			defer stopPID()
			fmt.Printf("ddm web UI on http://%s\n", displayAddr(ln.Addr().String()))
			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ln) }()
			select {
			case <-cmd.Context().Done():
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
				return nil
			case err := <-errCh:
				return err
			}
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "address to listen on (default: ui_address from the config)")
	return cmd
}

// displayAddr shows 0.0.0.0/[::] listeners as localhost.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || (host != "0.0.0.0" && host != "::") {
		return addr
	}
	return "localhost:" + port
}

// writeUIPID records the server's PID so `ddm service uninstall` can stop
// it on systems without a service manager.
func (g *globals) writeUIPID() func() {
	c, err := g.cache()
	if err != nil {
		return func() {}
	}
	path := filepath.Join(c.Root, "run", "ui.pid")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644) != nil {
		return func() {}
	}
	return func() { _ = os.Remove(path) }
}

// uiServer serves the page and its JSON API behind a password login.
type uiServer struct {
	g        *globals
	pwPath   string
	sessions *uiauth.Sessions
	throttle *uiauth.Throttle
	mux      *http.ServeMux

	pwMu  sync.Mutex
	pwMod time.Time // modification time of the password file last seen
}

func newUIServer(g *globals, pwPath string) *uiServer {
	s := &uiServer{
		g: g, pwPath: pwPath,
		sessions: uiauth.NewSessions(7 * 24 * time.Hour),
		throttle: &uiauth.Throttle{Max: 10, Window: time.Minute},
		mux:      http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /{$}", s.page)
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]string{"app": "ddm"}) })
	s.mux.HandleFunc("POST /api/login", s.login)
	s.mux.HandleFunc("POST /api/logout", s.logout)
	s.mux.HandleFunc("GET /api/me", s.authed(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]string{"version": Version}) }))
	s.mux.HandleFunc("POST /api/password", s.authed(s.changePassword))
	s.mux.HandleFunc("GET /api/config", s.authed(s.getConfig))
	s.mux.HandleFunc("POST /api/config", s.authed(s.setConfig))
	s.mux.HandleFunc("POST /api/config/unset", s.authed(s.unsetConfig))
	s.mux.HandleFunc("PUT /api/mirrors", s.authed(s.setMirrors))
	s.mux.HandleFunc("GET /api/status", s.authed(s.getStatus))
	s.mux.HandleFunc("POST /api/pause", s.authed(s.control("pause")))
	s.mux.HandleFunc("POST /api/resume", s.authed(s.control("resume")))
	s.mux.HandleFunc("POST /api/limit", s.authed(s.control("limit")))
	s.mux.HandleFunc("GET /api/cache", s.authed(s.getCache))
	s.mux.HandleFunc("POST /api/cache/gc", s.authed(s.cacheGC))
	return s
}

func (s *uiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every call that changes something must carry a custom header. Other
	// websites can't add one to a request without a CORS preflight, which
	// this server never approves, so they can't act through the browser.
	if r.Method != http.MethodGet && r.Header.Get("X-DDM") != "1" {
		writeJSONError(w, http.StatusForbidden, errors.New("missing X-DDM header"))
		return
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// syncPassword notices a password changed outside the UI (`ddm service
// password`) and logs every browser out.
func (s *uiServer) syncPassword() {
	fi, err := os.Stat(s.pwPath)
	if err != nil {
		return
	}
	s.pwMu.Lock()
	defer s.pwMu.Unlock()
	if !fi.ModTime().Equal(s.pwMod) {
		if !s.pwMod.IsZero() {
			s.sessions.RevokeAll()
		}
		s.pwMod = fi.ModTime()
	}
}

// authed lets a request through only with a valid session cookie.
func (s *uiServer) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.syncPassword()
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.Valid(c.Value) {
			writeJSONError(w, http.StatusUnauthorized, errors.New("log in first"))
			return
		}
		h(w, r)
	}
}

func (s *uiServer) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *uiServer) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiPage)
}

func (s *uiServer) login(w http.ResponseWriter, r *http.Request) {
	if !s.throttle.Allowed() {
		writeJSONError(w, http.StatusTooManyRequests, errors.New("too many failed logins; wait a minute"))
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	ok, err := uiauth.Check(s.pwPath, req.Password)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("reading the password: %w", err))
		return
	}
	if !ok {
		s.throttle.Fail()
		time.Sleep(500 * time.Millisecond)
		writeJSONError(w, http.StatusUnauthorized, errors.New("wrong password"))
		return
	}
	s.setSession(w, s.sessions.Create(), int((7 * 24 * time.Hour).Seconds()))
	writeJSON(w, map[string]string{"version": Version})
}

func (s *uiServer) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Revoke(c.Value)
	}
	s.setSession(w, "", -1)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *uiServer) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if ok, err := uiauth.Check(s.pwPath, req.Current); err != nil || !ok {
		writeJSONError(w, http.StatusBadRequest, errors.New("the current password is wrong"))
		return
	}
	if err := uiauth.Set(s.pwPath, req.New); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	// Log out every other browser; keep this one.
	s.syncPassword()
	s.sessions.RevokeAll()
	s.setSession(w, s.sessions.Create(), int((7 * 24 * time.Hour).Seconds()))
	writeJSON(w, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

// configFile re-reads the config on every request, so edits made with
// `ddm config` or by hand while the page is open are picked up.
func (s *uiServer) configFile() (*config.File, error) {
	path := s.g.configPath
	if path == "" {
		var err error
		if path, err = config.Path(); err != nil {
			return nil, err
		}
	}
	f, err := config.Load(path)
	if err == nil {
		s.g.cfg = f
	}
	return f, err
}

type settingJSON struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Default     string `json:"default"`
	Description string `json:"description"`
}

func (s *uiServer) getConfig(w http.ResponseWriter, _ *http.Request) {
	f, err := s.configFile()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	settings := make([]settingJSON, 0, len(config.Keys))
	for _, k := range config.Keys {
		v, _ := f.Get(k.Name)
		def, _ := config.DefaultValue(k.Name)
		settings = append(settings, settingJSON{Key: k.Name, Value: v, Default: def, Description: k.Description})
	}
	mirrors := f.Config.Mirrors
	if mirrors == nil {
		mirrors = map[string][]string{}
	}
	writeJSON(w, map[string]any{"path": f.Path, "settings": settings, "mirrors": mirrors})
}

func (s *uiServer) setConfig(w http.ResponseWriter, r *http.Request) {
	var req struct{ Key, Value string }
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	f, err := s.configFile()
	if err == nil {
		err = f.Set(req.Key, req.Value)
	}
	if err == nil {
		err = f.Save()
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.applyLive(req.Key, f)
	v, _ := f.Get(req.Key)
	writeJSON(w, map[string]string{"key": req.Key, "value": v})
}

func (s *uiServer) unsetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct{ Key string }
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	f, err := s.configFile()
	if err == nil {
		err = f.Unset(req.Key)
	}
	if err == nil {
		err = f.Save()
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	s.applyLive(req.Key, f)
	v, _ := f.Get(req.Key)
	writeJSON(w, map[string]string{"key": req.Key, "value": v})
}

// applyLive pushes a new bandwidth limit to downloads that are already
// running, so the setting takes effect at once, not only for the next one.
func (s *uiServer) applyLive(key string, f *config.File) {
	if key != "bandwidth_limit" {
		return
	}
	if _, d, err := s.g.runDir(); err == nil {
		_, _ = d.SetLimit(0, f.Config.RateLimit)
	}
}

func (s *uiServer) setMirrors(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Registry string   `json:"registry"`
		URLs     []string `json:"urls"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := upstreamRegistry(req.Registry); err != nil || req.Registry == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid registry %q", req.Registry))
		return
	}
	f, err := s.configFile()
	if err == nil {
		err = f.SetMirrors(req.Registry, req.URLs)
	}
	if err == nil {
		err = f.Save()
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"mirrors": f.Config.Mirrors})
}

func (s *uiServer) getStatus(w http.ResponseWriter, _ *http.Request) {
	runs, partials, err := s.g.collectStatus()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"runs": runs, "partials": partials})
}

// control handles pause, resume and limit for one PID, or all (pid 0).
func (s *uiServer) control(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PID  int    `json:"pid"`
			Rate string `json:"rate"`
		}
		if err := readJSON(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		_, d, err := s.g.runDir()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		var pids []int
		switch action {
		case "pause":
			pids, err = d.Pause(req.PID)
		case "resume":
			pids, err = d.Resume(req.PID)
		case "limit":
			var rate int64
			if rate, err = config.ParseSize(req.Rate); err == nil {
				pids, err = d.SetLimit(req.PID, rate)
			}
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if pids == nil {
			pids = []int{}
		}
		writeJSON(w, map[string]any{"pids": pids})
	}
}

func (s *uiServer) getCache(w http.ResponseWriter, _ *http.Request) {
	c, err := s.g.cache()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	refs, _ := c.Refs()
	blobs, _ := c.Blobs()
	partials, _ := c.Partials()
	referenced := map[string]bool{}
	for _, r := range refs {
		for _, b := range r.Blobs {
			referenced[b] = true
		}
	}
	var total, unref, partialBytes int64
	for _, b := range blobs {
		total += b.Size
		if !referenced[b.Digest] {
			unref += b.Size
		}
	}
	for _, p := range partials {
		partialBytes += p.Size
	}
	if refs == nil {
		refs = []cache.Ref{}
	}
	writeJSON(w, map[string]any{
		"root": c.Root, "refs": refs, "blobs": len(blobs), "bytes": total,
		"unreferenced_bytes": unref, "partials": len(partials), "partial_bytes": partialBytes,
	})
}

func (s *uiServer) cacheGC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThan string `json:"older_than"`
		All       bool   `json:"all"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	older, err := parseAge(req.OlderThan)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.g.cache()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	res, err := c.GC(cache.GCOptions{All: req.All, OlderThan: older, PartialAge: 14 * 24 * time.Hour, Grace: s.g.gcGrace(), DryRun: req.DryRun})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if res.Refs == nil {
		res.Refs = []string{}
	}
	writeJSON(w, map[string]any{"refs": res.Refs, "blobs": res.Blobs, "partials": res.Partials, "bytes": res.Bytes})
}
