package modelpull

import (
	"testing"

	"github.com/Hkarimi561/ddm/internal/cache"
	"github.com/Hkarimi561/ddm/internal/pull"
)

func TestImportWanted(t *testing.T) {
	p := &Puller{Images: &pull.Puller{Cache: &cache.Cache{Root: t.TempDir()}}}

	for mode, want := range map[string]bool{"auto": true, "on": true, "off": false, "": true} {
		p.HFImport = mode
		if got := p.importWanted(); got != want {
			t.Errorf("mode %q, no marker: importWanted = %v, want %v", mode, got, want)
		}
	}

	// After a failed import, auto stops importing; on keeps trying.
	p.markImportBroken()
	for mode, want := range map[string]bool{"auto": false, "on": true, "off": false} {
		p.HFImport = mode
		if got := p.importWanted(); got != want {
			t.Errorf("mode %q, marker set: importWanted = %v, want %v", mode, got, want)
		}
	}

	p.HFImport = "auto"
	p.clearImportBroken()
	if !p.importWanted() {
		t.Error("clearing the marker should turn importing back on")
	}
}
