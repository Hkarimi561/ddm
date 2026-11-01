// Package config reads and edits ddm's configuration file, a small YAML
// document meant to be edited by hand as well as with `ddm config` and
// `ddm mirror`. Edits go through the YAML node tree, so the user's comments
// and key order survive.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the effective configuration.
type Config struct {
	Connections  int
	Parallel     int
	Retries      int // 0 = retry forever
	StallTimeout time.Duration
	MinSpeed     int64 // bytes/s; a mirror slower than this is switched away from (0 = never)
	RateLimit    int64 // bytes/s for all downloads of a process together (0 = unlimited)
	UIAddress    string
	UICommands   bool   // allow pulling and running docker commands from the web UI
	HFImport     string // "auto", "on" or "off": load Hugging Face models into Model Runner from ddm's own download
	Proxy        string
	NoProxy      string
	CacheDir     string
	// Mirrors maps an upstream registry (as written in the file, e.g.
	// "docker.io") to mirror URLs in the order they are tried.
	Mirrors map[string][]string
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Connections:  4,
		Parallel:     3,
		StallTimeout: 30 * time.Second,
		MinSpeed:     32 << 10,
		UIAddress:    "127.0.0.1:7878",
		UICommands:   true,
		HFImport:     "auto",
		Mirrors:      map[string][]string{},
	}
}

// Key describes one scalar setting.
type Key struct {
	Name        string
	Description string
	get         func(*Config) string
	set         func(*Config, string) error
}

// Keys lists the settings `ddm config` knows about, in display order.
var Keys = []Key{
	{"connections", "parallel connections per blob",
		func(c *Config) string { return strconv.Itoa(c.Connections) },
		func(c *Config, v string) error { return setInt(&c.Connections, v, 1, 64) }},
	{"parallel", "blobs downloaded at the same time",
		func(c *Config) string { return strconv.Itoa(c.Parallel) },
		func(c *Config, v string) error { return setInt(&c.Parallel, v, 1, 32) }},
	{"retries", "consecutive failed attempts before giving up (0 = forever)",
		func(c *Config) string { return strconv.Itoa(c.Retries) },
		func(c *Config, v string) error { return setInt(&c.Retries, v, 0, 1<<30) }},
	{"stall_timeout", "reconnect when no data arrives for this long, e.g. 30s",
		func(c *Config) string { return c.StallTimeout.String() },
		func(c *Config, v string) error {
			d, err := time.ParseDuration(v)
			if err != nil || d < time.Second {
				return fmt.Errorf("want a duration of at least 1s, like 30s or 2m")
			}
			c.StallTimeout = d
			return nil
		}},
	{"min_speed", "switch to the next mirror below this speed, e.g. 32KiB (0 = never)",
		func(c *Config) string { return FormatSize(c.MinSpeed) },
		func(c *Config, v string) error {
			n, err := ParseSize(v)
			if err != nil {
				return err
			}
			c.MinSpeed = n
			return nil
		}},
	{"bandwidth_limit", "cap the total download speed, e.g. 2MiB (0 = unlimited)",
		func(c *Config) string { return FormatSize(c.RateLimit) },
		func(c *Config, v string) error {
			n, err := ParseSize(v)
			if err != nil {
				return err
			}
			c.RateLimit = n
			return nil
		}},
	{"ui_address", "where the web UI listens; 0.0.0.0:7878 opens it to your network (restart it after a change)",
		func(c *Config) string { return c.UIAddress },
		func(c *Config, v string) error {
			host, port, err := net.SplitHostPort(strings.TrimSpace(v))
			if p, perr := strconv.Atoi(port); err != nil || perr != nil || p < 1 || p > 65535 {
				return fmt.Errorf("want host:port, like 127.0.0.1:7878")
			}
			c.UIAddress = net.JoinHostPort(host, port)
			return nil
		}},
	{"ui_commands", "let the web UI pull images and run docker commands (true or false; applies at once)",
		func(c *Config) string { return strconv.FormatBool(c.UICommands) },
		func(c *Config, v string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("want true or false")
			}
			c.UICommands = b
			return nil
		}},
	{"hf_import", "Hugging Face models: on = download with ddm and import into Model Runner, off = let docker download them, auto = on until an import fails",
		func(c *Config) string { return c.HFImport },
		func(c *Config, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "auto" && v != "on" && v != "off" {
				return fmt.Errorf("want auto, on or off")
			}
			c.HFImport = v
			return nil
		}},
	{"proxy", "proxy for all registry traffic: http://, https:// or socks5:// (default: HTTP(S)_PROXY)",
		func(c *Config) string { return c.Proxy },
		func(c *Config, v string) error {
			if v != "" {
				if err := ValidateProxy(v); err != nil {
					return err
				}
			}
			c.Proxy = v
			return nil
		}},
	{"no_proxy", "comma-separated hosts that bypass the proxy (default: NO_PROXY)",
		func(c *Config) string { return c.NoProxy },
		func(c *Config, v string) error { c.NoProxy = v; return nil }},
	{"cache_dir", "where blobs and partial downloads are stored",
		func(c *Config) string { return c.CacheDir },
		func(c *Config, v string) error { c.CacheDir = v; return nil }},
}

const mirrorsKey = "mirrors"

func lookupKey(name string) (*Key, error) {
	for i := range Keys {
		if Keys[i].Name == name {
			return &Keys[i], nil
		}
	}
	names := make([]string, len(Keys))
	for i, k := range Keys {
		names[i] = k.Name
	}
	return nil, fmt.Errorf("unknown setting %q (known: %s; mirrors are managed with `ddm mirror`)", name, strings.Join(names, ", "))
}

func setInt(dst *int, v string, lo, hi int) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		return fmt.Errorf("want a whole number between %d and %d", lo, hi)
	}
	*dst = n
	return nil
}

// ValidateProxy checks a proxy URL.
func ValidateProxy(v string) error {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid proxy URL %q", v)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		return nil
	}
	return fmt.Errorf("unsupported proxy scheme %q (use http, https or socks5)", u.Scheme)
}

// Path returns $DDM_CONFIG, or config.yaml in ddm's directory under the OS
// config directory (~/.config/ddm on Linux, ~/Library/Application Support/ddm
// on macOS, %AppData%\ddm on Windows).
func Path() (string, error) {
	if p := os.Getenv("DDM_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ddm", "config.yaml"), nil
}

// File is a loaded configuration file.
type File struct {
	Path   string
	Config Config
	doc    yaml.Node
}

// Load reads the file at path. A missing file is not an error: it yields
// the defaults, and Save creates it.
func Load(path string) (*File, error) {
	f := &File{Path: path, Config: Defaults()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &f.doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	root := f.root(false)
	if root == nil {
		return f, nil
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: expected a mapping of settings at the top level", path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value == mirrorsKey {
			if err := v.Decode(&f.Config.Mirrors); err != nil {
				return nil, fmt.Errorf("%s line %d: mirrors must map a registry to a list of URLs: %w", path, v.Line, err)
			}
			if f.Config.Mirrors == nil {
				f.Config.Mirrors = map[string][]string{}
			}
			continue
		}
		key, err := lookupKey(k.Value)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, k.Line, err)
		}
		if v.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s line %d: %s must be a single value", path, v.Line, key.Name)
		}
		if err := key.set(&f.Config, v.Value); err != nil {
			return nil, fmt.Errorf("%s line %d: %s: %w", path, v.Line, key.Name, err)
		}
	}
	for reg, urls := range f.Config.Mirrors {
		for _, u := range urls {
			if _, err := ParseMirrorURL(u); err != nil {
				return nil, fmt.Errorf("%s: mirrors for %s: %w", path, reg, err)
			}
		}
	}
	return f, nil
}

// DefaultValue returns a setting's built-in default.
func DefaultValue(name string) (string, error) {
	k, err := lookupKey(name)
	if err != nil {
		return "", err
	}
	d := Defaults()
	return k.get(&d), nil
}

// Get returns the effective value of a setting.
func (f *File) Get(name string) (string, error) {
	k, err := lookupKey(name)
	if err != nil {
		return "", err
	}
	return k.get(&f.Config), nil
}

// Set validates and stores a setting. Call Save to write it.
func (f *File) Set(name, value string) error {
	k, err := lookupKey(name)
	if err != nil {
		return err
	}
	if err := k.set(&f.Config, value); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	setScalar(f.root(true), name, value)
	return nil
}

// Unset restores a setting's default.
func (f *File) Unset(name string) error {
	k, err := lookupKey(name)
	if err != nil {
		return err
	}
	_ = k.set(&f.Config, k.get(ptr(Defaults())))
	if root := f.root(false); root != nil {
		deleteKey(root, name)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// Mirrors returns the mirror list configured for an upstream registry, and
// the key it is stored under.
func (f *File) Mirrors(registry string) (key string, urls []string) {
	want := NormalizeRegistry(registry)
	for k, v := range f.Config.Mirrors {
		if NormalizeRegistry(k) == want {
			return k, v
		}
	}
	return registry, nil
}

// SetMirrors replaces the mirror list of an upstream registry. An empty list
// removes the entry.
func (f *File) SetMirrors(registry string, urls []string) error {
	for _, u := range urls {
		if _, err := ParseMirrorURL(u); err != nil {
			return err
		}
	}
	key, _ := f.Mirrors(registry)
	delete(f.Config.Mirrors, key)

	root := f.root(true)
	mirrors := mapValue(root, mirrorsKey)
	if mirrors == nil || mirrors.Kind != yaml.MappingNode {
		if len(urls) == 0 {
			return nil
		}
		mirrors = &yaml.Node{Kind: yaml.MappingNode}
		setNode(root, mirrorsKey, mirrors)
	}
	if len(urls) == 0 {
		deleteKey(mirrors, key)
		if len(mirrors.Content) == 0 {
			deleteKey(root, mirrorsKey)
		}
		return nil
	}
	f.Config.Mirrors[key] = urls
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, u := range urls {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: u})
	}
	setNode(mirrors, key, seq)
	return nil
}

// MirrorRegistries returns the configured upstream registries, sorted.
func (f *File) MirrorRegistries() []string {
	var out []string
	for k := range f.Config.Mirrors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Save writes the file atomically, creating its directory if needed.
func (f *File) Save() error {
	if f.root(false) == nil {
		f.root(true)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&f.doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// root returns the top-level mapping, creating it (with a header comment)
// when create is set.
func (f *File) root(create bool) *yaml.Node {
	if f.doc.Kind == yaml.DocumentNode && len(f.doc.Content) > 0 {
		return f.doc.Content[0]
	}
	if !create {
		return nil
	}
	m := &yaml.Node{Kind: yaml.MappingNode}
	f.doc = yaml.Node{
		Kind:        yaml.DocumentNode,
		HeadComment: "ddm configuration. Edit freely, or use `ddm config set` and `ddm mirror add`.",
		Content:     []*yaml.Node{m},
	}
	return m
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// Keep comments attached to the old value.
			v.HeadComment, v.LineComment, v.FootComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment, m.Content[i+1].FootComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}

func setScalar(m *yaml.Node, key, value string) {
	setNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
