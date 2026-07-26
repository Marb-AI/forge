package ui

import (
	"regexp"
	"strings"
	"testing"
)

// buttonRe matches a whole <button …>…</button>, including its contents.
var buttonRe = regexp.MustCompile(`(?s)<button\b([^>]*)>(.*?)</button>`)

// tagRe strips markup, leaving whatever text a control actually shows.
var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// Every control that shows an icon and no words must say what it does.
//
// An icon is a guess until something names it. The rail buttons carry a caption
// and are still ambiguous enough to want one ("host" — whose host?); a bare glyph
// in a panel header has nothing at all. This is the rule that is easy to keep
// while writing the markup and easy to forget while editing it, which is why it
// is a test and not a convention.
func TestEveryIconOnlyControlHasATooltip(t *testing.T) {
	html := embeddedAsset(t, "index.html")
	for _, m := range buttonRe.FindAllStringSubmatch(html, -1) {
		attrs, body := m[1], m[2]
		// A control with real words in it explains itself.
		if text := strings.TrimSpace(tagRe.ReplaceAllString(body, "")); len(text) > 2 {
			continue
		}
		if strings.Contains(attrs, "title=") || strings.Contains(attrs, "aria-label=") {
			continue
		}
		id := "«no id»"
		if idm := regexp.MustCompile(`id="([^"]+)"`).FindStringSubmatch(attrs); idm != nil {
			id = idm[1]
		}
		t.Errorf("icon-only button %s has no title: %s", id, strings.TrimSpace(m[0]))
	}
}

// The same rule for the rows the browser builds: they are truncated by design —
// a name that does not fit is clipped, a figure is abbreviated — so each has to
// carry the full version somewhere. These are the builders that abbreviate.
func TestEveryAbbreviatedValueCarriesItsFullForm(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	builders := map[string]string{
		"portRow":     "row.title = portTitle(p, st)",
		"serverRow":   "name.title = serverTitle(s)",
		"metric":      "el.title = title",
		"loginGroupR": "row.title = loginTitle(g)",
	}
	for name, assign := range builders {
		if !strings.Contains(js, assign) {
			t.Errorf("%s no longer sets a tooltip (%q missing)", name, assign)
		}
	}
	// The two identity lines truncate a server name and an email respectively.
	for _, want := range []string{"serverLine.title =", "loginLine.title ="} {
		if !strings.Contains(js, want) {
			t.Errorf("an identity line has no tooltip (%q missing)", want)
		}
	}
	// And the port panel's own buttons, which are icon-only by construction.
	if !strings.Contains(js, `b.title = stop ? `) {
		t.Error("the container start/stop button has no title")
	}
}

// A metric shows an abbreviated figure ("41/63") whose unit lives only in the
// tooltip, so a metric built without one is a number about nothing.
func TestMetricRequiresATitle(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	calls := regexp.MustCompile(`metric\(ICON_[A-Z]+,`).FindAllString(js, -1)
	if len(calls) != 3 {
		t.Fatalf("expected CPU, memory and disk metrics, found %d", len(calls))
	}
	// Each call site passes four arguments, the last being the tooltip.
	for _, unit := range []string{"CPU not measured", "RAM not measured", "Disk not measured"} {
		if !strings.Contains(js, unit) {
			t.Errorf("no tooltip for the unmeasured case of %q", unit)
		}
	}
	if !strings.Contains(js, "% used · ") || !strings.Contains(js, " free") {
		t.Error("the memory and disk tooltips should say the percentage and what is left")
	}
}
