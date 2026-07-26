package forge

import (
	"go/build"
	"strings"
	"testing"
)

// The core must not import a front end. That is the whole point of it existing:
// the CLI and the browser UI are adapters over this package, and an import going
// the other way would put an operation back inside one of them — which is where
// they were, and why the UI could only borrow them.
//
// The compiler catches it TODAY, because both front ends import this package and
// the cycle would not build. That is not the case this guards: it guards the day
// one of them stops importing the core directly, at which point the cycle is gone
// and nothing but this test says which way the dependency is supposed to point.
func TestCoreDoesNotImportAFrontEnd(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bad := frontEnds(append(pkg.Imports, pkg.TestImports...)); len(bad) > 0 {
		t.Errorf("the core imports a front end: %s — the operation belongs here, not "+
			"there, and the dependency must point this way", strings.Join(bad, ", "))
	}
}

// And the check itself, against imports the package cannot currently have (the
// cycle would not compile), so a rule that has stopped matching anything cannot
// pass for a rule that is satisfied.
func TestFrontEndImportsAreRecognised(t *testing.T) {
	got := frontEnds([]string{
		"sort",
		"github.com/Marb-AI/forge/config",
		"github.com/Marb-AI/forge/internal/agentproto",
		"github.com/Marb-AI/forge/internal/ui",
		"github.com/Marb-AI/forge/internal/cli",
	})
	if len(got) != 2 {
		t.Errorf("front ends found = %v; want both ui and cli", got)
	}
}

// frontEnds picks out the imports that point at a front end.
func frontEnds(imports []string) []string {
	var bad []string
	for _, imp := range imports {
		if strings.HasSuffix(imp, "/internal/ui") || strings.HasSuffix(imp, "/internal/cli") {
			bad = append(bad, imp)
		}
	}
	return bad
}
