package cli

import (
	"github.com/Marb-AI/forge/forge"
	"github.com/Marb-AI/forge/internal/ui"
)

// The two hidden subcommands the detached daemons re-exec themselves with. They
// are not commands anyone types, which is why they are absent from the usage
// text: `forge spawn` and `forge ui` launch them.
//
// Both are one call and an exit code. The daemon a front end starts must be the
// same daemon whatever started it, so what they run lives behind the seam — the
// supervisor in the core, the browser UI in its own package.

// runSupervisor is the foreground body of the detached forwarding daemon.
func runSupervisor() int {
	if err := forge.RunSupervisor(); err != nil {
		return fail("%v", err)
	}
	return 0
}

// runUI is the foreground body of the detached browser UI daemon.
func runUI() int {
	if err := ui.Run(); err != nil {
		return fail("%v", err)
	}
	return 0
}
