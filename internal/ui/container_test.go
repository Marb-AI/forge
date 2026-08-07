package ui

import (
	"os"
	"regexp"
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

	// And neither may quietly grow one the other has not heard of.
	dev := regexp.MustCompile(`lib[a-z0-9.-]+-dev`)
	inDocker := set(dev.FindAllString(dockerfile, -1))
	inCI := set(dev.FindAllString(workflow, -1))
	for pkg := range inCI {
		if !inDocker[pkg] {
			t.Errorf("CI installs %s and the container does not — the container would "+
				"pass what CI fails", pkg)
		}
	}
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
