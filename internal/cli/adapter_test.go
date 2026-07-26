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
// package held until it became an adapter, plus a third-party one, since "not
// ours" is not the same as "harmless". A rule that has stopped matching anything
// passes for a rule that is satisfied.
func TestTheRuleRecognisesWhatItForbids(t *testing.T) {
	allowed := []string{
		"fmt",
		"text/tabwriter",
		"os/exec", // stdlib, and the adapter does open a browser with it
		"github.com/Marb-AI/forge/forge",
		"github.com/Marb-AI/forge/internal/clip",
		"github.com/Marb-AI/forge/internal/ui",
	}
	forbidden := []string{
		"github.com/Marb-AI/forge/config",
		"github.com/Marb-AI/forge/internal/sshx",
		"github.com/Marb-AI/forge/internal/supervisor",
		"github.com/Marb-AI/forge/internal/agentproto",
		"github.com/Marb-AI/forge/internal/proc",
		// Nothing about a dependency being someone else's makes it an adapter's:
		// a CLI that grew its own ssh client would be as far from this rule as one
		// that reached for ours.
		"golang.org/x/crypto/ssh",
		"github.com/creack/pty",
	}
	if got := beyondTheAdapter(allowed); len(got) > 0 {
		t.Errorf("the rule rejects what an adapter may have: %v", got)
	}
	if got := beyondTheAdapter(forbidden); len(got) != len(forbidden) {
		t.Errorf("the rule caught %v; want all %d of %v", got, len(forbidden), forbidden)
	}
}

// beyondTheAdapter picks out the imports an adapter has no business having:
// anything that is not the standard library or one of the three below.
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
		if stdlib(imp) || allowed[imp] {
			continue
		}
		bad = append(bad, imp)
	}
	return bad
}

// stdlib reports whether an import path is a standard library package. The test
// go/build itself uses: a domain in the first element is what makes a path
// external, and the standard library has none.
func stdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
