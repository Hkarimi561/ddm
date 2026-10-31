package cli

import (
	"strings"
	"testing"
)

func TestShellInit(t *testing.T) {
	for shell, want := range map[string]string{
		"bash":       `command ddm docker "$@"`,
		"zsh":        `command ddm docker "$@"`,
		"fish":       "command ddm docker $argv",
		"powershell": "ddm docker @args",
	} {
		s, err := shellInit(shell)
		if err != nil {
			t.Fatalf("%s: %v", shell, err)
		}
		if !strings.Contains(s, want) {
			t.Errorf("%s script lacks %q:\n%s", shell, want, s)
		}
	}
	if _, err := shellInit("tcsh"); err == nil {
		t.Error("unsupported shell accepted")
	}
}
