package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/ui"
)

func newCacheCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and clean up the download cache",
		Long: `ddm keeps downloaded blobs in its cache, so shared layers are downloaded
once and interrupted downloads can resume. Images and models stay in the
cache after they are loaded into Docker; "ddm cache gc" frees the space.`,
	}
	cmd.AddCommand(newCacheLsCmd(g), newCacheRmCmd(g), newCacheGCCmd(g))
	return cmd
}

func newCacheLsCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List cached images and models",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.cache()
			if err != nil {
				return err
			}
			refs, err := c.Refs()
			if err != nil {
				return err
			}
			blobs, err := c.Blobs()
			if err != nil {
				return err
			}
			partials, _ := c.Partials()
			referenced := map[string]bool{}
			for _, r := range refs {
				for _, b := range r.Blobs {
					referenced[b] = true
				}
			}
			var total, unref int64
			for _, b := range blobs {
				total += b.Size
				if !referenced[b.Digest] {
					unref += b.Size
				}
			}
			var partialBytes int64
			for _, p := range partials {
				partialBytes += p.Size
			}
			if asJSON {
				return printJSON(map[string]any{
					"root": c.Root, "refs": refs, "blobs": len(blobs), "bytes": total,
					"unreferenced_bytes": unref, "partials": len(partials), "partial_bytes": partialBytes,
				})
			}
			if len(refs) == 0 {
				fmt.Println("Nothing cached yet.")
			} else {
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tKIND\tSIZE\tLAST USED")
				for _, r := range refs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Kind, ui.HumanBytes(r.Size), ago(r.Used))
				}
				tw.Flush()
			}
			fmt.Printf("\n%s in %d blobs", ui.HumanBytes(total), len(blobs))
			if unref > 0 {
				fmt.Printf(" (%s not used by anything listed)", ui.HumanBytes(unref))
			}
			fmt.Println(".")
			if len(partials) > 0 {
				fmt.Printf("%d unfinished download(s) reserving %s; see `ddm status`.\n", len(partials), ui.HumanBytes(partialBytes))
			}
			fmt.Printf("Cache: %s\n", c.Root)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newCacheRmCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME...",
		Short: "Remove images or models from the cache (not from Docker)",
		Long: `Forgets cached images or models (names as shown by "ddm cache ls") and
deletes the blobs nothing else uses. What is already loaded into Docker
stays there.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := g.cache()
			if err != nil {
				return err
			}
			for _, name := range args {
				ok, err := c.Forget(name)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("%s is not in the cache (see `ddm cache ls`)", name)
				}
			}
			res, err := c.GC(cache.GCOptions{Grace: g.gcGrace()})
			if err != nil {
				return err
			}
			fmt.Printf("Removed %s; freed %s.\n", strings.Join(args, ", "), ui.HumanBytes(res.Bytes))
			return nil
		},
	}
}

func newCacheGCCmd(g *globals) *cobra.Command {
	var all, dryRun bool
	var olderThan, partialAge string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Free space: delete blobs nothing needs and old unfinished downloads",
		Long: `Deletes cached blobs that no listed image or model uses, unfinished
downloads untouched for --partials-older-than, and leftovers of interrupted
runs. Downloads in progress are never touched.

With --older-than, images and models not used for that long are forgotten
first. With --all, the whole cache is emptied (except running downloads).`,
		Example: `  ddm cache gc
  ddm cache gc --older-than 30d
  ddm cache gc --all --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			older, err := parseAge(olderThan)
			if err != nil {
				return fmt.Errorf("--older-than: %w", err)
			}
			partials, err := parseAge(partialAge)
			if err != nil {
				return fmt.Errorf("--partials-older-than: %w", err)
			}
			c, err := g.cache()
			if err != nil {
				return err
			}
			res, err := c.GC(cache.GCOptions{All: all, OlderThan: older, PartialAge: partials, Grace: g.gcGrace(), DryRun: dryRun})
			if err != nil {
				return err
			}
			verb := "Freed"
			if dryRun {
				verb = "Would free"
			}
			fmt.Printf("%s %s: %d blob(s), %d unfinished download(s)", verb, ui.HumanBytes(res.Bytes), res.Blobs, res.Partials)
			if len(res.Refs) > 0 {
				fmt.Printf(", forgetting %s", strings.Join(res.Refs, ", "))
			}
			fmt.Println(".")
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&all, "all", false, "empty the whole cache")
	f.StringVar(&olderThan, "older-than", "", "also forget images and models unused for this long, e.g. 30d")
	f.StringVar(&partialAge, "partials-older-than", "14d", "delete unfinished downloads untouched for this long (0 = keep)")
	f.BoolVar(&dryRun, "dry-run", false, "only report what would be deleted")
	return cmd
}

// gcGrace protects freshly downloaded blobs from gc while a pull is still
// running (it records what it downloaded only at the end).
func (g *globals) gcGrace() time.Duration {
	if _, d, err := g.runDir(); err == nil {
		if runs, _ := d.List(); len(runs) == 0 {
			return 0
		}
	}
	return time.Hour
}

// parseAge accepts Go durations plus days ("30d"); "" and "0" mean zero.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid age %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (examples: 7d, 12h)", s)
	}
	return d, nil
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
