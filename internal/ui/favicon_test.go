package ui

import (
	"io/fs"
	"strings"
	"testing"
)

// The favicon has to be *in the binary*. Forge ships as one file; an icon that
// lived on disk, or worse on a CDN, would be an empty tab for everyone who just
// downloaded a release.
func TestFaviconIsEmbedded(t *testing.T) {
	// Read it out of the embedded FS specifically, not the dev-mode disk one.
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(sub, "favicon.svg")
	if err != nil {
		t.Fatalf("the favicon is not compiled into the binary: %v", err)
	}

	svg := string(data)
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
	sub, _ := fs.Sub(assetsFS, "assets")
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `rel="icon"`) {
		t.Error("index.html does not link the favicon")
	}
	if !strings.Contains(string(index), "/assets/favicon.svg") {
		t.Error("index.html links a favicon that isn't the one we ship")
	}
}
