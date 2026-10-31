package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init bash|zsh|fish|powershell",
		Short: "Print a shell function that sends docker commands through ddm",
		Long: `Prints a "docker" shell function that runs "ddm docker", so plain docker
commands download their images with ddm first. Nothing changes until you
load it from your shell's startup file:

  bash        echo 'eval "$(ddm shell-init bash)"' >> ~/.bashrc
  zsh         echo 'eval "$(ddm shell-init zsh)"' >> ~/.zshrc
  fish        echo 'ddm shell-init fish | source' >> ~/.config/fish/config.fish
  PowerShell  Add-Content $PROFILE 'ddm shell-init powershell | Out-String | Invoke-Expression'

To bypass ddm once: "command docker ..." (bash, zsh, fish) or "docker.exe ..."
(PowerShell). To turn it off for a session: set DDM_PASSTHROUGH=off.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := shellInit(args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), s)
			return nil
		},
	}
}

func shellInit(shell string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return `# docker via ddm (https://github.com/Hkarimi561/ddm). Bypass with: command docker ...
docker() { command ddm docker "$@"; }
`, nil
	case "fish":
		return `# docker via ddm (https://github.com/Hkarimi561/ddm). Bypass with: command docker ...
function docker --wraps docker --description 'docker via ddm'
    command ddm docker $argv
end
`, nil
	case "powershell", "pwsh":
		return `# docker via ddm (https://github.com/Hkarimi561/ddm). Bypass with: docker.exe ...
function docker { ddm docker @args }
`, nil
	}
	return "", fmt.Errorf("unsupported shell %q (use bash, zsh, fish or powershell)", shell)
}
