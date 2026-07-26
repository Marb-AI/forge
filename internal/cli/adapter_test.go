package cli

import (
	"go/build"
	"strings"
	"testing"
)

// The CLI is an adapter: argv in, one call to the core, text out. It does not
// reach a server, read the config, or start a process of its own — every one of
// those is an operation, and operations live in package forge, where the browser
// UI reaches the same one.
//
// That is a property no compiler checks. The CLI could import sshx tomorrow and
// everything would still build, and the second front end would quietly stop
// being a front end over the same actions and start being a reimplementation of
// them — which is exactly the state this initiative moved out of. So the rule is
// written as the list of packages an adapter is allowed to know about.
func TestTheCLIReachesForgeOnlyThroughTheCore(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad := beyondTheAdapter(pkg.Imports); len(bad) > 0 {
		t.Errorf("the CLI imports %s — whatever it needs from there is an operation, "+
			"and belongs in package forge where both front ends can reach it",
			strings.Join(bad, ", "))
	}
}

// And the rule itself, against the imports it exists to catch — the ones this
// package held until it became an adapter. A rule that has stopped matching
// anything passes for a rule that is satisfied.
func TestTheRuleRecognisesWhatItForbids(t *testing.T) {
	got := beyondTheAdapter([]string{
		"fmt",
		"text/tabwriter",
		"github.com/Marb-AI/forge/forge",
		"github.com/Marb-AI/forge/internal/clip",
		"github.com/Marb-AI/forge/internal/ui",
		"github.com/Marb-AI/forge/config",
		"github.com/Marb-AI/forge/internal/sshx",
		"github.com/Marb-AI/forge/internal/supervisor",
		"github.com/Marb-AI/forge/internal/agentproto",
		"github.com/Marb-AI/forge/internal/proc",
	})
	if len(got) != 5 {
		t.Errorf("forbidden imports found = %v; want the five that are not stdlib, "+
			"the core, the clipboard filter or the UI daemon", got)
	}
}

// beyondTheAdapter picks out the imports an adapter has no business having.
//
// The three it may have: the core, the clipboard filter (which exists because of
// the terminal this front end is attached to, and means nothing to any other),
// and the UI package — because `forge ui` spawns that daemon, and this is the
// process entry point it re-execs into.
func beyondTheAdapter(imports []string) []string {
	const mod = "github.com/Marb-AI/forge/"
	allowed := map[string]bool{
		mod + "forge":         true,
		mod + "internal/clip": true,
		mod + "internal/ui":   true,
	}
	var bad []string
	for _, imp := range imports {
		if !strings.HasPrefix(imp, mod) || allowed[imp] {
			continue // stdlib, or one of ours that an adapter may have
		}
		bad = append(bad, imp)
	}
	return bad
}
