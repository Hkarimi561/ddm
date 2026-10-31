package cli

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/registry"
	"github.com/Hkarimi561/ddm/internal/ui"
)

func newMirrorCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage registry mirrors",
		Long: `Mirrors are tried in order before the upstream registry itself. If a mirror
keeps failing, does not have a blob, or is slower than min_speed, ddm moves
on to the next one and resumes where it left off.

The upstream is always tried last unless you place it explicitly, e.g.
"ddm mirror add docker.io https://docker.io".`,
		Example: `  ddm mirror add docker.io https://docker.arvancloud.ir
  ddm mirror add ghcr.io https://harbor.example.com/ghcr-proxy
  ddm mirror test docker.io --apply
  ddm mirror list`,
	}
	cmd.AddCommand(
		newMirrorListCmd(g),
		newMirrorAddCmd(g),
		newMirrorRemoveCmd(g),
		newMirrorSetDefaultCmd(g),
		newMirrorTestCmd(g),
	)
	return cmd
}

// upstreamRegistry parses a registry argument such as "docker.io".
func upstreamRegistry(s string) (name.Registry, error) {
	reg, err := name.NewRegistry(config.NormalizeRegistry(s))
	if err != nil {
		return name.Registry{}, fmt.Errorf("invalid registry %q: %w", s, err)
	}
	return reg, nil
}

func sameMirror(a, b string) bool {
	ma, errA := config.ParseMirrorURL(a)
	mb, errB := config.ParseMirrorURL(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ma.Host == mb.Host && ma.Prefix == mb.Prefix && ma.Insecure == mb.Insecure
}

func indexOfMirror(urls []string, u string) int {
	return slices.IndexFunc(urls, func(x string) bool { return sameMirror(x, u) })
}

type mirrorEntry struct {
	URL      string `json:"url"`
	Upstream bool   `json:"upstream,omitempty"`
}

func newMirrorListCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list [REGISTRY]",
		Short: "Show configured mirrors in the order they are tried",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			regs := f.MirrorRegistries()
			if len(args) == 1 {
				key, _ := f.Mirrors(args[0])
				regs = []string{key}
			}
			rc := &registry.Client{Mirrors: f.Config.Mirrors}
			out := map[string][]mirrorEntry{}
			for _, r := range regs {
				reg, err := upstreamRegistry(r)
				if err != nil {
					return err
				}
				for _, ep := range rc.Endpoints(reg) {
					out[r] = append(out[r], mirrorEntry{URL: ep.Name, Upstream: ep.Upstream})
				}
			}
			if asJSON {
				return printJSON(out)
			}
			if len(regs) == 0 {
				fmt.Println("No mirrors configured. Add one with: ddm mirror add docker.io https://mirror.example.com")
				return nil
			}
			for _, r := range regs {
				fmt.Printf("%s:\n", r)
				for i, e := range out[r] {
					suffix := ""
					if e.Upstream {
						suffix = "  (upstream)"
					}
					fmt.Printf("  %d. %s%s\n", i+1, e.URL, suffix)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newMirrorAddCmd(g *globals) *cobra.Command {
	var first bool
	cmd := &cobra.Command{
		Use:   "add REGISTRY URL",
		Short: "Add a mirror for a registry",
		Long: `Add a mirror for a registry. URL may carry a path for registries that serve a
proxy cache under a project, e.g. https://harbor.example.com/dockerhub.
Credentials for a mirror come from "docker login <mirror-host>".`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			reg, u := args[0], args[1]
			if _, err := upstreamRegistry(reg); err != nil {
				return err
			}
			if _, err := config.ParseMirrorURL(u); err != nil {
				return err
			}
			key, urls := f.Mirrors(reg)
			if indexOfMirror(urls, u) >= 0 {
				return fmt.Errorf("%s is already a mirror of %s", u, key)
			}
			pos := len(urls)
			if first {
				pos = 0
			} else if up := slices.IndexFunc(urls, func(x string) bool {
				m, err := config.ParseMirrorURL(x)
				return err == nil && m.IsUpstream(reg)
			}); up >= 0 {
				pos = up // keep an explicitly placed upstream after the mirrors
			}
			urls = slices.Insert(slices.Clone(urls), pos, u)
			if err := f.SetMirrors(key, urls); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Printf("Added %s as mirror #%d of %s.\n", u, pos+1, key)
			return nil
		},
	}
	cmd.Flags().BoolVar(&first, "first", false, "try this mirror before the others")
	return cmd
}

func newMirrorRemoveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "remove REGISTRY URL",
		Aliases: []string{"rm"},
		Short:   "Remove a mirror",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			key, urls := f.Mirrors(args[0])
			i := indexOfMirror(urls, args[1])
			if i < 0 {
				return fmt.Errorf("%s is not a mirror of %s (see `ddm mirror list`)", args[1], key)
			}
			if err := f.SetMirrors(key, slices.Delete(slices.Clone(urls), i, i+1)); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Printf("Removed %s from the mirrors of %s.\n", args[1], key)
			return nil
		},
	}
}

func newMirrorSetDefaultCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "set-default REGISTRY URL",
		Short: "Make a mirror the first one tried (adding it if needed)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			if _, err := config.ParseMirrorURL(args[1]); err != nil {
				return err
			}
			key, urls := f.Mirrors(args[0])
			urls = slices.Clone(urls)
			if i := indexOfMirror(urls, args[1]); i >= 0 {
				urls = slices.Delete(urls, i, i+1)
			}
			urls = slices.Insert(urls, 0, args[1])
			if err := f.SetMirrors(key, urls); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Printf("%s is now tried first for %s.\n", args[1], key)
			return nil
		},
	}
}

// defaultTestImages are used by `ddm mirror test` when --image is not given.
// They are large enough for a meaningful speed sample.
var defaultTestImages = map[string]string{
	config.DockerHub: "library/ubuntu:latest",
}

func newMirrorTestCmd(g *globals) *cobra.Command {
	var (
		image, platformStr, sample string
		maxTime                    time.Duration
		apply, asJSON              bool
	)
	cmd := &cobra.Command{
		Use:   "test [REGISTRY]",
		Short: "Measure each mirror's latency and speed and suggest an order",
		Long: `Fetch a test image's manifest and part of its largest layer from every
mirror of a registry (and the registry itself), then suggest the fastest
order. With --apply, the suggested order is saved.`,
		Example: `  ddm mirror test                     # docker.io, using ubuntu:latest
  ddm mirror test docker.io --apply
  ddm mirror test --image ghcr.io/org/app:latest`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			regArg := "docker.io"
			if len(args) == 1 {
				regArg = args[0]
			}
			reg, err := upstreamRegistry(regArg)
			if err != nil {
				return err
			}
			if image == "" {
				img, ok := defaultTestImages[config.NormalizeRegistry(reg.RegistryStr())]
				if !ok {
					return fmt.Errorf("there is no default test image for %s; pass one with --image", regArg)
				}
				image = reg.RegistryStr() + "/" + img
			}
			ref, err := name.ParseReference(image)
			if err != nil {
				return fmt.Errorf("--image: %w", err)
			}
			if len(args) == 1 && config.NormalizeRegistry(ref.Context().RegistryStr()) != config.NormalizeRegistry(reg.RegistryStr()) {
				return fmt.Errorf("--image %s is not on %s", image, regArg)
			}
			reg = ref.Context().Registry
			if platformStr == "" {
				platformStr = "linux/" + runtime.GOARCH
			}
			platform, err := v1.ParsePlatform(platformStr)
			if err != nil {
				return fmt.Errorf("--platform: %w", err)
			}
			sampleBytes, err := config.ParseSize(sample)
			if err != nil || sampleBytes <= 0 {
				return fmt.Errorf("--sample: want a size like 8MiB")
			}

			rc, err := g.registryClient(g.logger())
			if err != nil {
				return err
			}
			eps := rc.Endpoints(reg)
			if !asJSON {
				fmt.Printf("Testing %d endpoint(s) for %s with %s (up to %s or %s each)\n\n",
					len(eps), regArg, ref, ui.HumanBytes(sampleBytes), maxTime)
			}
			var probes []registry.Probe
			for _, ep := range eps {
				if !asJSON {
					fmt.Printf("  %s ...", ep.Name)
				}
				pr := rc.ProbeEndpoint(cmd.Context(), ep, ref, *platform, sampleBytes, maxTime)
				if cmd.Context().Err() != nil {
					return cmd.Context().Err()
				}
				if !asJSON {
					if pr.Err != nil {
						fmt.Println(" failed")
					} else {
						fmt.Printf(" %s/s\n", ui.HumanBytes(int64(pr.Speed())))
					}
				}
				probes = append(probes, pr)
			}

			ranked := rankProbes(probes)
			if asJSON {
				return printJSON(probeReport(ranked))
			}
			fmt.Println()
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "RANK\tENDPOINT\tLATENCY\tSPEED\tRESULT")
			for i, pr := range ranked {
				latency, speed, result := pr.Latency.Round(time.Millisecond).String(), ui.HumanBytes(int64(pr.Speed()))+"/s", "ok"
				if pr.Err != nil {
					speed, result = "-", pr.Err.Error()
				}
				endpoint := pr.Endpoint.Name
				if pr.Endpoint.Upstream {
					endpoint += " (upstream)"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i+1, endpoint, latency, speed, truncate(result, 60))
			}
			tw.Flush()

			if len(eps) == 1 {
				fmt.Println("\nOnly the upstream is configured; add mirrors with `ddm mirror add`.")
				return nil
			}
			order := suggestedOrder(ranked)
			if !apply {
				fmt.Println("\nRun again with --apply to save this order.")
				return nil
			}
			f, err := g.config()
			if err != nil {
				return err
			}
			key, _ := f.Mirrors(reg.RegistryStr())
			if err := f.SetMirrors(key, order); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			fmt.Printf("\nSaved the new order for %s.\n", key)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&image, "image", "", "image to test with (default: ubuntu:latest for docker.io)")
	fl.StringVar(&platformStr, "platform", "", "platform of the test image (default: linux/<this machine's arch>)")
	fl.StringVar(&sample, "sample", "8MiB", "how much of a layer to download from each endpoint")
	fl.DurationVar(&maxTime, "time", 15*time.Second, "maximum download time per endpoint")
	fl.BoolVar(&apply, "apply", false, "save the suggested order")
	fl.BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// rankProbes orders working endpoints by speed (fastest first, latency
// breaking ties) and puts failed ones last in their original order.
func rankProbes(probes []registry.Probe) []registry.Probe {
	ranked := slices.Clone(probes)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if (a.Err == nil) != (b.Err == nil) {
			return a.Err == nil
		}
		if a.Err != nil {
			return false
		}
		if a.Speed() != b.Speed() {
			return a.Speed() > b.Speed()
		}
		return a.Latency < b.Latency
	})
	return ranked
}

// suggestedOrder turns ranked probes into a mirror list. The upstream is
// written out only when it is not last, since it is implied at the end.
func suggestedOrder(ranked []registry.Probe) []string {
	var order []string
	for i, pr := range ranked {
		if pr.Endpoint.Upstream {
			if i == len(ranked)-1 {
				continue
			}
			order = append(order, "https://"+pr.Endpoint.Name)
			continue
		}
		order = append(order, pr.Endpoint.Name)
	}
	fmt.Println()
	fmt.Println("Suggested order:")
	for i, pr := range ranked {
		fmt.Printf("  %d. %s\n", i+1, pr.Endpoint.Name)
	}
	return order
}

type probeJSON struct {
	Endpoint    string  `json:"endpoint"`
	Upstream    bool    `json:"upstream,omitempty"`
	LatencyMS   int64   `json:"latency_ms"`
	BytesPerSec float64 `json:"bytes_per_sec"`
	Bytes       int64   `json:"bytes"`
	Error       string  `json:"error,omitempty"`
}

func probeReport(ranked []registry.Probe) []probeJSON {
	out := make([]probeJSON, 0, len(ranked))
	for _, pr := range ranked {
		p := probeJSON{
			Endpoint:    pr.Endpoint.Name,
			Upstream:    pr.Endpoint.Upstream,
			LatencyMS:   pr.Latency.Milliseconds(),
			BytesPerSec: pr.Speed(),
			Bytes:       pr.Bytes,
		}
		if pr.Err != nil {
			p.Error = pr.Err.Error()
		}
		out = append(out, p)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
