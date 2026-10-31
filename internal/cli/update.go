package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/registry"
)

const defaultRepo = "Hkarimi561/ddm"

// githubBase is a variable so tests can point it at a local server.
var githubBase = "https://github.com"

func newUpdateCmd(g *globals) *cobra.Command {
	var check, force bool
	var target string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update ddm to the latest release",
		Long: `Downloads the latest release for this OS and CPU from GitHub, checks it
against the release's checksums.txt, replaces the running ddm binary and
restarts the web UI if it is installed. Use --check to only look for a
new version, and --version to install a specific release (also to go
back to an older one). $DDM_REPO picks another GitHub repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd.Context(), g, target, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether a newer version exists")
	cmd.Flags().BoolVar(&force, "force", false, "install even if this version is already current (or a development build)")
	cmd.Flags().StringVar(&target, "version", "", "install this release tag, e.g. v0.5.1 (default: latest)")
	return cmd
}

func runUpdate(ctx context.Context, g *globals, target string, check, force bool) error {
	cfg, err := g.config()
	if err != nil {
		return err
	}
	repo := os.Getenv("DDM_REPO")
	if repo == "" {
		repo = defaultRepo
	}
	client := &http.Client{Transport: registry.NewTransport(registry.ProxyFunc(cfg.Config.Proxy, cfg.Config.NoProxy))}

	tag := target
	if tag == "" {
		if tag, err = latestTag(ctx, client, repo); err != nil {
			return err
		}
	} else if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}

	cmp := compareVersions(Version, tag)
	switch {
	case target != "":
		// An explicit version is always installed unless it's the current one.
		if cmp == 0 && !force {
			fmt.Printf("ddm %s is already installed.\n", Version)
			return nil
		}
	case cmp >= 0 && Version != "dev" && !force:
		fmt.Printf("ddm %s is up to date (latest release: %s).\n", Version, tag)
		return nil
	}
	if check {
		fmt.Printf("ddm %s is available (you have %s). Run `ddm update` to install it.\n", tag, Version)
		return nil
	}
	if Version == "dev" && !force {
		return errors.New("this is a development build; use `ddm update --force` to replace it with " + tag)
	}

	exe, err := executable()
	if err != nil {
		return err
	}
	fmt.Printf("Updating ddm %s -> %s\n", Version, tag)
	if err := replaceBinary(ctx, client, repo, tag, exe); err != nil {
		return err
	}
	fmt.Printf("Updated %s to %s.\n", exe, tag)

	if serviceInstalled() {
		g.stopUI()
		if err := exec.Command(exe, "service", "install").Run(); err != nil {
			fmt.Println("The web UI could not be restarted; run `ddm service install`.")
		} else {
			fmt.Println("Restarted the web UI.")
		}
	}
	return nil
}

// latestTag follows GitHub's /releases/latest redirect, which needs no API
// token and has no rate limit.
func latestTag(ctx context.Context, client *http.Client, repo string) (string, error) {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, githubBase+"/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking for updates: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || !strings.Contains(loc, "/releases/tag/") {
		return "", fmt.Errorf("checking for updates: %s has no published release (HTTP %d)", repo, resp.StatusCode)
	}
	return path.Base(loc), nil
}

func assetName() string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "ddm_" + runtime.GOOS + "_" + runtime.GOARCH + ext
}

func replaceBinary(ctx context.Context, client *http.Client, repo, tag, exe string) error {
	dir := filepath.Dir(exe)
	// Next to the binary, so the final rename stays on one filesystem.
	tmp, err := os.MkdirTemp(dir, ".ddm-update-")
	if err != nil {
		return fmt.Errorf("can't write to %s (%w); run the installer again instead", dir, err)
	}
	defer os.RemoveAll(tmp)

	asset := assetName()
	base := githubBase + "/" + repo + "/releases/download/" + tag
	archive := filepath.Join(tmp, asset)
	if err := download(ctx, client, base+"/"+asset, archive); err != nil {
		return err
	}
	sums := filepath.Join(tmp, "checksums.txt")
	if err := download(ctx, client, base+"/checksums.txt", sums); err != nil {
		return err
	}
	if err := verifyChecksum(archive, sums, asset); err != nil {
		return err
	}

	name, next := "ddm", filepath.Join(dir, ".ddm.new")
	if runtime.GOOS == "windows" {
		name, next = "ddm.exe", filepath.Join(dir, "ddm.exe.new")
	}
	if err := extractBinary(archive, name, next); err != nil {
		return err
	}
	defer os.Remove(next)

	// Run it once before swapping, so a binary that can't start (antivirus,
	// wrong platform) never replaces a working one.
	out, err := exec.CommandContext(ctx, next, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded ddm can't run (is antivirus blocking it?): %w", err)
	}
	if !strings.Contains(string(out), strings.TrimPrefix(tag, "v")) {
		return fmt.Errorf("the downloaded ddm reports %q, expected %s", strings.TrimSpace(string(out)), tag)
	}
	return swapBinary(exe, next)
}

func download(ctx context.Context, client *http.Client, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	return f.Close()
}

func verifyChecksum(file, sums, asset string) error {
	b, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}
	r, err := os.Open(file)
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", asset, want, got)
	}
	return nil
}

// extractBinary writes the file called name from a .zip or .tar.gz to dest.
func extractBinary(archive, name, dest string) error {
	var src io.Reader
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name == name {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				src = rc
				break
			}
		}
	} else {
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Name == name {
				src = tr
				break
			}
		}
	}
	if src == nil {
		return fmt.Errorf("%s does not contain %s", filepath.Base(archive), name)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// compareVersions compares dotted numeric versions, ignoring a leading "v"
// and any "-suffix". It returns -1, 0 or 1; a non-numeric version such as
// "dev" is older than any release.
func compareVersions(a, b string) int {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
