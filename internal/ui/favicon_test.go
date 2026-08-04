package ui

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// embeddedAsset reads a file out of the *embedded* FS specifically — not the
// dev-mode disk one, which would happily serve an asset that never made it into
// the binary. Every failure on the way is reported here, so a caller can't end up
// dereferencing a nil FS and panicking somewhere less obvious.
func embeddedAsset(t *testing.T, name string) string {
	t.Helper()
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		t.Fatalf("the embedded assets are not where they should be: %v", err)
	}
	data, err := fs.ReadFile(sub, name)
	if err != nil {
		t.Fatalf("%s is not compiled into the binary: %v", name, err)
	}
	return string(data)
}

// The favicon has to be *in the binary*. Forge ships as one file; an icon that
// lived on disk, or worse on a CDN, would be an empty tab for everyone who just
// downloaded a release.
func TestFaviconIsEmbedded(t *testing.T) {
	svg := embeddedAsset(t, "favicon.svg")

	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "</svg>") {
		t.Error("favicon.svg is not an SVG")
	}
	if !strings.Contains(svg, "viewBox") {
		t.Error("favicon.svg needs a viewBox, or it won't scale to the tab")
	}
	// A single flat fill goes invisible on one of the two browser chromes.
	if !strings.Contains(svg, "prefers-color-scheme: dark") {
		t.Error("favicon.svg must adapt to a light and a dark tab strip")
	}
	// It is drawn at 16px more often than anywhere else; keep it a shape, not a
	// scene.
	if n := strings.Count(svg, "<path"); n != 1 {
		t.Errorf("favicon should be one path (it renders at 16px), found %d", n)
	}
}

// …and the page has to point at it, or embedding it achieves nothing.
func TestIndexLinksTheFavicon(t *testing.T) {
	index := embeddedAsset(t, "index.html")

	if !strings.Contains(index, `rel="icon"`) {
		t.Error("index.html does not link the favicon")
	}
	if !strings.Contains(index, "/assets/favicon.svg") {
		t.Error("index.html links a favicon that isn't the one we ship")
	}
}

// …and the app icon has to be the same anvil.
//
// There are two of them now: this SVG, drawn by the browser, and the .icns the
// Finder shows for Forge.app, which build/icon.py rasterises from a copy of this
// path. A copy is what makes the icon a script with no dependencies rather than
// a build tool nobody has installed — the shape is one closed polygon of straight
// segments, and that is the whole trick. The cost of a copy is that it can drift,
// and drift here means Forge is one mark in the tab and a different one in the
// Dock, which nobody would notice until both are on screen at once.
//
// So the two are held to the same points. Redrawing the favicon fails this test,
// which is the reminder to run build/icon.py's list past the same pen.
func TestTheAppIconIsTheSameAnvil(t *testing.T) {
	svg := embeddedAsset(t, "favicon.svg")

	// A leading space, because `id="` ends in the same three characters and would
	// hand this test an attribute that is not a path.
	d, ok := attr(svg, ` d="`)
	if !ok {
		t.Fatal("favicon.svg has no path data")
	}
	drawn := points(d)
	if len(drawn) < 3 {
		t.Fatalf("read %d points out of the favicon path %q", len(drawn), d)
	}

	script, err := os.ReadFile(filepath.Join("..", "..", "build", "icon.py"))
	if err != nil {
		t.Fatalf("the app icon is not generated from anything: %v", err)
	}
	list, ok := between(string(script), "ANVIL = [", "]")
	if !ok {
		t.Fatal("build/icon.py has no ANVIL to compare against")
	}
	rendered := points(list)
	// Same floor as the favicon side. Without it a list this test can no longer
	// read — reformatted, or written some other way — reports as the two marks
	// having drifted apart, which sends whoever reads it to the wrong file.
	if len(rendered) < 3 {
		t.Fatalf("read %d points out of build/icon.py's ANVIL %q — that is this test "+
			"failing to read the list, not the two marks disagreeing",
			len(rendered), list)
	}

	if !slices.Equal(drawn, rendered) {
		t.Errorf("the tab and the Dock would show different marks:\n"+
			"  favicon.svg   %v\n  build/icon.py %v\n"+
			"whichever one is right, the other has to follow", drawn, rendered)
	}
}

// points pulls every x,y pair out of either notation — the SVG's "M1 10 L6 6" or
// the script's "(1, 10), (6, 6)" — by reading the numbers in order and pairing
// them. Neither file has a number in it that is not a coordinate.
func points(s string) [][2]int {
	var nums []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r < '0' || r > '9'
	}) {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil
		}
		nums = append(nums, n)
	}
	var out [][2]int
	for i := 0; i+1 < len(nums); i += 2 {
		out = append(out, [2]int{nums[i], nums[i+1]})
	}
	return out
}

func attr(s, key string) (string, bool) { return between(s, key, `"`) }
func between(s, open, close string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
