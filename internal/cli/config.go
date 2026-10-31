package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Hkarimi561/ddm/internal/config"
)

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change settings",
		Long: `Settings live in a YAML file you can also edit by hand (see "ddm config path").
Command-line flags override them for a single run. Mirrors are managed
with "ddm mirror".`,
	}

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "Show every setting with its current value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			if asJSON {
				out := map[string]any{}
				for _, k := range config.Keys {
					v, _ := f.Get(k.Name)
					out[k.Name] = v
				}
				out["mirrors"] = f.Config.Mirrors
				return printJSON(out)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			for _, k := range config.Keys {
				v, _ := f.Get(k.Name)
				if v == "" {
					v = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t# %s\n", k.Name, v, k.Description)
			}
			tw.Flush()
			if len(f.Config.Mirrors) > 0 {
				fmt.Println("\nmirrors: see `ddm mirror list`")
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	get := &cobra.Command{
		Use:   "get KEY",
		Short: "Print one setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			v, err := f.Get(args[0])
			if err != nil {
				return err
			}
			fmt.Println(v)
			return nil
		},
	}

	set := &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Change a setting",
		Example: `  ddm config set connections 8
  ddm config set proxy socks5://127.0.0.1:1080
  ddm config set min_speed 64KiB
  ddm config set stall_timeout 1m`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			if err := f.Set(args[0], args[1]); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			v, _ := f.Get(args[0])
			fmt.Printf("%s = %s\n", args[0], v)
			return nil
		},
	}

	unset := &cobra.Command{
		Use:   "unset KEY",
		Short: "Restore a setting's default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			if err := f.Unset(args[0]); err != nil {
				return err
			}
			if err := f.Save(); err != nil {
				return err
			}
			v, _ := f.Get(args[0])
			fmt.Printf("%s = %s (default)\n", args[0], v)
			return nil
		},
	}

	path := &cobra.Command{
		Use:   "path",
		Short: "Print the location of the config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := g.config()
			if err != nil {
				return err
			}
			fmt.Println(f.Path)
			return nil
		},
	}

	cmd.AddCommand(list, get, set, unset, path)
	return cmd
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
