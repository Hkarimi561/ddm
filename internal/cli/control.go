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
	"github.com/Hkarimi561/ddm/internal/config"
	"github.com/Hkarimi561/ddm/internal/engine"
	"github.com/Hkarimi561/ddm/internal/runstate"
	"github.com/Hkarimi561/ddm/internal/ui"
)

// runDir opens the run directory of the configured cache.
func (g *globals) runDir() (*cache.Cache, runstate.Dir, error) {
	c, err := g.cache()
	if err != nil {
		return nil, "", err
	}
	d, err := runstate.Open(c.Root)
	return c, d, err
}

type partialJSON struct {
	Digest string `json:"digest"`
	Done   int64  `json:"done"`
	Size   int64  `json:"size"`
	Active bool   `json:"active"`
}

func newStatusCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show running downloads and unfinished ones that can be resumed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runs, partials, err := g.collectStatus()
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(map[string]any{"runs": runs, "partials": partials})
			}
			printStatus(runs, partials)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// collectStatus gathers running downloads and unfinished ones in the cache.
func (g *globals) collectStatus() ([]runstate.Status, []partialJSON, error) {
	c, d, err := g.runDir()
	if err != nil {
		return nil, nil, err
	}
	runs, err := d.List()
	if err != nil {
		return nil, nil, err
	}
	active := map[string]bool{}
	for _, r := range runs {
		for _, it := range r.Items {
			if it.State == "downloading" {
				active[it.Digest] = true
			}
		}
	}
	partials := []partialJSON{}
	if ps, err := c.Partials(); err == nil {
		for _, p := range ps {
			done, size, _, err := engine.ReadProgress(p.Paths.State)
			if err != nil {
				size = p.Size
			}
			partials = append(partials, partialJSON{Digest: p.Digest, Done: done, Size: size, Active: active[p.Digest]})
		}
	}
	if runs == nil {
		runs = []runstate.Status{}
	}
	return runs, partials, nil
}

func printStatus(runs []runstate.Status, partials []partialJSON) {
	if len(runs) == 0 {
		fmt.Println("No downloads running.")
	}
	for _, r := range runs {
		state := "downloading"
		if r.Paused {
			state = "PAUSED"
		}
		pct := 0.0
		if r.Total > 0 {
			pct = 100 * float64(r.Done) / float64(r.Total)
		}
		fmt.Printf("PID %d  %s\n", r.PID, r.Command)
		line := fmt.Sprintf("  %s: %s / %s (%.0f%%)", state, ui.HumanBytes(r.Done), ui.HumanBytes(r.Total), pct)
		if !r.Paused && r.BytesPerSec > 0 {
			line += fmt.Sprintf(", %s/s", ui.HumanBytes(int64(r.BytesPerSec)))
			if left := r.Total - r.Done; left > 0 {
				line += fmt.Sprintf(", about %s left", (time.Duration(float64(left)/r.BytesPerSec) * time.Second).Round(time.Second))
			}
		}
		if r.RateLimit > 0 {
			line += fmt.Sprintf(" (limited to %s/s)", ui.HumanBytes(r.RateLimit))
		}
		fmt.Println(line)
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, it := range r.Items {
			src := ""
			if it.Source != "" && it.State == "downloading" {
				src = "via " + it.Source
			}
			fmt.Fprintf(tw, "    %s\t%s / %s\t%s\t%s\n", it.Name, ui.HumanBytes(it.Done), ui.HumanBytes(it.Size), it.State, src)
		}
		tw.Flush()
	}

	var idle []partialJSON
	for _, p := range partials {
		if !p.Active {
			idle = append(idle, p)
		}
	}
	if len(idle) > 0 {
		var done, size int64
		for _, p := range idle {
			done += p.Done
			size += p.Size
		}
		fmt.Printf("\n%d unfinished download(s) in the cache (%s of %s); they resume automatically the next time the image or model is pulled.\n",
			len(idle), ui.HumanBytes(done), ui.HumanBytes(size))
	}
}

// pidArg parses an optional PID argument (0 = all).
func pidArg(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid PID %q", args[0])
	}
	return pid, nil
}

func pidList(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ", ")
}

func newPauseCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "pause [PID]",
		Short: "Pause running downloads (all, or the one with PID)",
		Long: `Pauses running downloads within a second. Open connections are closed and
everything downloaded so far is kept; "ddm resume" continues exactly there.
A paused download can also be stopped with Ctrl+C and resumed later by
running the same command again.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pidArg(args)
			if err != nil {
				return err
			}
			_, d, err := g.runDir()
			if err != nil {
				return err
			}
			pids, err := d.Pause(pid)
			if err != nil {
				return err
			}
			if len(pids) == 0 {
				fmt.Println("No downloads running.")
				return nil
			}
			fmt.Printf("Paused PID %s. Resume with `ddm resume`.\n", pidList(pids))
			return nil
		},
	}
}

func newResumeCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "resume [PID]",
		Short: "Resume paused downloads (all, or the one with PID)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := pidArg(args)
			if err != nil {
				return err
			}
			_, d, err := g.runDir()
			if err != nil {
				return err
			}
			pids, err := d.Resume(pid)
			if err != nil {
				return err
			}
			if len(pids) == 0 {
				fmt.Println("No downloads running. Unfinished downloads resume when you run their pull command again.")
				return nil
			}
			fmt.Printf("Resumed PID %s.\n", pidList(pids))
			return nil
		},
	}
}

func newLimitCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "limit RATE [PID]",
		Short: "Change the speed limit of running downloads, e.g. 2MiB or 0 for none",
		Long: `Changes the bandwidth limit of running downloads (all, or the one with PID)
within a second. For future downloads use --limit-rate, or make it the
default with "ddm config set bandwidth_limit 2MiB".`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rate, err := config.ParseSize(args[0])
			if err != nil {
				return err
			}
			pid, err := pidArg(args[1:])
			if err != nil {
				return err
			}
			_, d, err := g.runDir()
			if err != nil {
				return err
			}
			pids, err := d.SetLimit(pid, rate)
			if err != nil {
				return err
			}
			if len(pids) == 0 {
				fmt.Println("No downloads running.")
				return nil
			}
			what := "no limit"
			if rate > 0 {
				what = ui.HumanBytes(rate) + "/s"
			}
			fmt.Printf("PID %s: %s.\n", pidList(pids), what)
			return nil
		},
	}
}
