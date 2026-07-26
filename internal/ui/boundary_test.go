package ui

import (
	"go/build"
	"strings"
	"testing"
)

// The browser UI is a front end: HTTP in, HTTP out, and every operation asked of
// package forge. It does not reach a workspace, a host, or a process of its own —
// it used to, and terminals were the last of it: this package built the ssh argv
// itself, started it under a pty, and the file browser wrote its own remote shell
// snippets. Both are the core's now, and what is left here moves bytes.
//
// No compiler checks that. The handlers would still build if one of them
// imported os/exec tomorrow, and the second front end would quietly stop being a
// front end over the same actions and start being a reimplementation of them. So
// the rule is written as the list of packages this one is allowed to know about.
//
// What it deliberately still allows: the daemon's own process — binding a port,
// claiming a pidfile, catching a signal, serving embedded files. That is this
// package being a program, not this package reaching a server, and it is the next
// thing to move when where-things-are-stored becomes something the core answers.
func TestTheUIReachesForgeOnlyThroughTheCore(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad := beyondAFrontEnd(pkg.Imports); len(bad) > 0 {
		t.Errorf("the UI imports %s — whatever it needs from there is an operation, "+
			"and belongs in package forge where both front ends reach the same one",
			strings.Join(bad, ", "))
	}
}

// And the rule itself, against the imports it exists to catch — the ones this
// package held until the terminals and the file browser moved out. A rule that
// has stopped matching anything passes for a rule that is satisfied.
func TestTheRuleRecognisesWhatAFrontEndMayNotHave(t *testing.T) {
	allowed := []string{
		"net/http",
		"encoding/json",
		"os", // the daemon's own pidfile and token still live here
		"github.com/Marb-AI/forge/forge",
		"github.com/Marb-AI/forge/config",
	}
	forbidden := []string{
		// Starting a process is how this package used to open a terminal.
		"os/exec",
		"github.com/Marb-AI/forge/internal/sshx",
		"github.com/Marb-AI/forge/internal/agentproto",
		"github.com/Marb-AI/forge/internal/supervisor",
		// Nothing about a dependency being someone else's makes it a front end's: a
		// UI that grew its own pty, or its own ssh client, would be as far from this
		// rule as one that reached for ours.
		"github.com/creack/pty",
		"golang.org/x/crypto/ssh",
	}
	if got := beyondAFrontEnd(allowed); len(got) > 0 {
		t.Errorf("the rule rejects what a front end may have: %v", got)
	}
	if got := beyondAFrontEnd(forbidden); len(got) != len(forbidden) {
		t.Errorf("the rule caught %v; want all %d of %v", got, len(forbidden), forbidden)
	}
}

// beyondAFrontEnd picks out the imports a front end has no business having: the
// core and the config are its own, the standard library is fine except for the
// part that starts processes, and everything else is somebody's operation.
func beyondAFrontEnd(imports []string) []string {
	const mod = "github.com/Marb-AI/forge/"
	allowed := map[string]bool{
		mod + "forge":  true,
		mod + "config": true,
	}
	var bad []string
	for _, imp := range imports {
		// os/exec is stdlib and still forbidden: starting a process is how this
		// package used to open a terminal, and it is the one thing a front end must
		// ask for rather than do.
		if imp != "os/exec" && (allowed[imp] || isStdlib(imp)) {
			continue
		}
		bad = append(bad, imp)
	}
	return bad
}

// isStdlib reports whether an import path is a standard library package. The test
// go/build itself uses: a domain in the first element is what makes a path
// external, and the standard library has none.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
