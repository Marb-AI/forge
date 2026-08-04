package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// What is left to test here is the dispatch: that a word on the command line
// reaches the right operation, and that the exit code says what happened. The
// operations themselves are the core's, and are tested there — running them
// through argv only tested them twice, more slowly.
func TestMainRouting(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"help"}, 0},
		{[]string{"bogus"}, 2},
		{[]string{"host"}, 1},
		{[]string{"host", "bogus"}, 1},
		{[]string{"workspace"}, 1},
		{[]string{"workspace", "x"}, 1},          // an action is required
		{[]string{"workspace", "x", "bogus"}, 1}, // and has to be one of ours
		{[]string{"forwarding"}, 1},
		{[]string{"forwarding", "bogus"}, 1},
		{[]string{"show"}, 1},
		{[]string{"show", "bogus"}, 1},
		{[]string{"ports", "bogus"}, 1},
		{[]string{"ui", "bogus"}, 1},
		{[]string{"ui", "port"}, 1},           // needs a port
		{[]string{"ui", "port", "http"}, 1},   // and it has to be a number
		{[]string{"workspace", "list"}, 0},    // no workspaces registered
		{[]string{"forwarding", "status"}, 0}, // no supervisor
		{[]string{"ui", "status"}, 0},         // not running
		// -q says the same thing in the exit code and nothing on stdout, which is
		// what the installer reads to know which daemons to start again.
		{[]string{"ui", "status", "-q"}, 1},
		{[]string{"forwarding", "status", "-q"}, 1},
		{[]string{"show", "ports"}, 0}, // no hosts
		{[]string{"host", "list"}, 0},  // no hosts
		{[]string{"version"}, 0},       // this client's build
		{[]string{"--version"}, 0},     // the spelling people try first
		{[]string{"-v"}, 0},
		{[]string{"version", "nope"}, 1}, // a host nobody registered
	}
	for _, c := range cases {
		if got := Main(c.args); got != c.want {
			t.Errorf("Main(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

// `host add` is the one command whose argv shape is worth testing through the
// dispatcher: the alias is a flag that may sit on either side of the target, and
// both halves are required.
func TestHostAddArguments(t *testing.T) {
	if got := Main([]string{"host", "add", "root@1.2.3.4", "--alias=srv"}); got != 0 {
		t.Fatalf("host add = %d", got)
	}
	if got := Main([]string{"host", "add", "--alias=srv2", "root@1.2.3.4"}); got != 0 {
		t.Errorf("--alias before the target should work, got %d", got)
	}
	if got := Main([]string{"host", "add", "root@h"}); got != 1 {
		t.Errorf("missing --alias should fail, got %d", got)
	}
	if got := Main([]string{"host", "add", "--alias=only"}); got != 1 {
		t.Errorf("missing target should fail, got %d", got)
	}
	for _, alias := range []string{"srv", "srv2"} {
		if got := Main([]string{"host", "remove", alias}); got != 0 {
			t.Errorf("host remove %s = %d", alias, got)
		}
	}
}

// The ports panel offers every published port as a link to this machine, and each
// of those is carried by a tunnel the forwarding supervisor holds. Starting the UI
// without them leaves a panel full of addresses that do not answer — with no sign
// but a browser tab that times out — so `forge ui` brings them up.
//
// Not the other way round: stopping the UI leaves the tunnels alone. They serve
// this machine, not the window.
func TestStartingTheUIBringsTheTunnelsUpAndStoppingItDoesNot(t *testing.T) {
	start := funcBody(t, "ui.go", "uiStart")
	if !strings.Contains(start, "startTunnels()") {
		t.Error("`forge ui` starts no tunnels — its ports panel would link to addresses nothing answers on")
	}
	if body := funcBody(t, "ui.go", "startTunnels"); !strings.Contains(body, "RestartForwarding") {
		t.Errorf("startTunnels does not start the supervisor: %q", body)
	}
	if stop := funcBody(t, "ui.go", "uiStop"); strings.Contains(stop, "Forwarding") {
		t.Error("`forge ui stop` touches the tunnels — they outlive the window that showed them")
	}
}

// funcBody returns a top-level function's source from one of this package's files.
//
// Parsed rather than matched: the obvious version looks for the next line that is
// a lone closing brace, which is correct only for as long as every file stays
// gofmt'd and no string literal in one holds a "}" at the start of a line. The
// parser knows where a function ends, so this cannot be wrong about it.
func funcBody(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
	}
	t.Fatalf("%s has no func %s", file, name)
	return ""
}
