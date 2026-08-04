package main

import (
	"go/build"
	"strings"
	"testing"
)

// The desktop shell is a window and nothing else.
//
// It is the newest place where the boundary Iniciativa 1 drew could quietly come
// undone, and the easiest: a shell has a real UI toolkit in its hands, so the
// tempting next move is always to do one small thing natively — read the config
// to show the servers in a menu, open an ssh session for a "connect" button,
// stat a path for a file dialog. Each of those is an operation, each already
// exists in package forge, and each done here would exist twice: once for this
// shell, once for the browser, and a third time for the phone.
//
// So the rule is the same one the CLI carries, and for the same reason: the
// packages a shell is allowed to know about, written down, because no compiler
// will ever object. This shell needs exactly one of ours — the UI, whose Start
// hands back a URL — and after that its whole job is a window.
func TestTheDesktopShellOnlyStartsTheUI(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad := beyondTheShell(pkg.Imports); len(bad) > 0 {
		t.Errorf("the desktop shell imports %s — whatever it needs from there is an "+
			"operation, and belongs in package forge where every front end reaches it",
			strings.Join(bad, ", "))
	}
}

// And the rule against what it exists to catch. A guard that has stopped matching
// anything reads exactly like a guard that is satisfied.
func TestTheRuleRecognisesWhatItForbids(t *testing.T) {
	allowed := []string{
		"fmt",
		"os",
		"github.com/Marb-AI/forge/internal/ui",
		"github.com/wailsapp/wails/v3/pkg/application",
		"github.com/wailsapp/wails/v3/pkg/events",
	}
	forbidden := []string{
		// The core itself included: a shell that calls an operation directly has
		// started deciding what the UI shows, and the browser will not have it.
		"github.com/Marb-AI/forge/forge",
		"github.com/Marb-AI/forge/config",
		"github.com/Marb-AI/forge/keys",
		"github.com/Marb-AI/forge/internal/sshx",
		"github.com/Marb-AI/forge/internal/supervisor",
		// Nothing about a dependency being someone else's makes it a shell's.
		"golang.org/x/crypto/ssh",
		"github.com/creack/pty",
	}
	if got := beyondTheShell(allowed); len(got) > 0 {
		t.Errorf("the rule rejects what a shell may have: %v", got)
	}
	if got := beyondTheShell(forbidden); len(got) != len(forbidden) {
		t.Errorf("the rule caught %v; want all %d of %v", got, len(forbidden), forbidden)
	}
}

// beyondTheShell picks out the imports a shell has no business having: anything
// that is not the standard library, the UI it starts, or the toolkit that gives
// it a window.
func beyondTheShell(imports []string) []string {
	var bad []string
	for _, imp := range imports {
		switch {
		case !strings.Contains(strings.SplitN(imp, "/", 2)[0], "."): // stdlib
		case imp == "github.com/Marb-AI/forge/internal/ui":
		case strings.HasPrefix(imp, "github.com/wailsapp/wails/v3/"):
		default:
			bad = append(bad, imp)
		}
	}
	return bad
}
