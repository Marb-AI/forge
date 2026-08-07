package ui

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The container and CI must agree on what cmd/forge-app compiles against.
//
// Both install the same three -dev packages, in two files, and nothing but this
// keeps them the same. A container that disagreed with CI would be the worse
// half of a bad bargain: it would pass what CI fails, so the run that is
// supposed to be the safe one becomes the one that lies.
//
// Held as a set rather than as text, because the two have different reasons to
// be written differently — one is a workflow step, the other a Dockerfile layer.
func TestTheContainerAndCIInstallTheSamePackages(t *testing.T) {
	dockerfile := readRepoFile(t, "build/Dockerfile")
	workflow := readRepoFile(t, ".github/workflows/ci.yml")

	// The packages this is about, named here so that adding one to a single file
	// fails rather than passes: the list is the thing being checked.
	want := []string{"libgtk-4-dev", "libwebkitgtk-6.0-dev", "libx11-dev"}

	for _, pkg := range want {
		if !strings.Contains(dockerfile, pkg) {
			t.Errorf("build/Dockerfile does not install %s, so the container cannot "+
				"build cmd/forge-app and `go vet ./...` there covers less than CI", pkg)
		}
		if !strings.Contains(workflow, pkg) {
			t.Errorf("ci.yml does not install %s", pkg)
		}
	}

	// And CI's list is exactly this one, so adding a package there without adding
	// it here fails. Without that the `want` list is a floor rather than the
	// answer, and two files could drift together past a test that says they
	// cannot.
	//
	// Only CI is held to the exact set: the Dockerfile legitimately installs more
	// (a compiler, pkg-config, libc6-dev), which the workflow gets from the
	// runner image.
	dev := regexp.MustCompile(`lib[a-z0-9.-]+-dev`)
	inCI := set(dev.FindAllString(workflow, -1))
	if len(inCI) != len(want) {
		t.Errorf("CI installs %v; this test knows about %v — one of them has moved on",
			keys(inCI), want)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func set(items []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range items {
		out[s] = true
	}
	return out
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}
