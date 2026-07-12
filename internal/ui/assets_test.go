package ui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// zIndexOf finds the z-index a selector is given in the stylesheet, taking the
// last one to win — which is what the cascade does at equal specificity.
//
// Comments are stripped first. Without that, the selector of a rule that follows a
// comment comes back with the comment glued to the front and matches nothing — the
// first version of this helper reported that #wizard had no z-index at all, which
// was a bug in the test, not in the stylesheet.
func zIndexOf(t *testing.T, css, selector string) int {
	t.Helper()
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")

	rule := regexp.MustCompile(`(?s)([^{}]*)\{([^}]*)\}`)
	z := regexp.MustCompile(`z-index:\s*(\d+)`)

	found := -1
	for _, m := range rule.FindAllStringSubmatch(css, -1) {
		selectors, body := m[1], m[2]
		hit := false
		for _, s := range strings.Split(selectors, ",") {
			if strings.TrimSpace(s) == selector {
				hit = true
			}
		}
		if !hit {
			continue
		}
		if zm := z.FindStringSubmatch(body); zm != nil {
			n, err := strconv.Atoi(zm[1])
			if err != nil {
				t.Fatalf("bad z-index for %s: %v", selector, err)
			}
			found = n
		}
	}
	if found < 0 {
		t.Fatalf("%s has no z-index", selector)
	}
	return found
}

// The confirm dialog is opened BY the other modals — "Delete" inside Settings asks
// for it. It therefore has to paint above them. It didn't: all three shared one
// z-index, so DOM order decided, and #confirm lost — you got a delete confirmation
// hidden underneath the panel that asked for it.
func TestConfirmDialogPaintsAboveTheOtherModals(t *testing.T) {
	css := embeddedAsset(t, "app.css")

	confirm := zIndexOf(t, css, "#confirm")
	for _, below := range []string{"#settings", "#wizard"} {
		if z := zIndexOf(t, css, below); confirm <= z {
			t.Errorf("#confirm (z-index %d) must paint above %s (z-index %d) — it is opened by it",
				confirm, below, z)
		}
	}
}

// Every status Go can produce must be one the browser knows how to say. The two
// live on opposite sides of a JSON boundary with nothing but a shared string to
// hold them together: rename the constant in Go and, with no test here, the UI
// would go on quietly labelling every workspace "server unreachable".
func TestBrowserUnderstandsEveryStatusGoCanSend(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	// The ones that must be named outright, because each reads differently to a
	// person and one of them must not offer a Start button.
	for _, status := range []string{
		agentproto.StatusRunning,
		agentproto.StatusStopped,
		agentproto.StatusMissing,
	} {
		if !strings.Contains(js, `case "`+status+`"`) {
			t.Errorf("app.js does not handle the %q status", status)
		}
	}

	// Unreachable is the fallback, so it needs no case of its own — but a workspace
	// on a host we cannot reach must still not look startable.
	if !strings.Contains(js, `function isUsable(`) {
		t.Fatal("app.js has no isUsable(): a missing or unreachable workspace would be offered a Start button")
	}
	for _, usable := range []string{agentproto.StatusRunning, agentproto.StatusStopped} {
		if !strings.Contains(js, `status === "`+usable+`"`) {
			t.Errorf("isUsable should treat %q as usable", usable)
		}
	}
}

// The status the agent reports is `tmux has-session -t claude`: it describes the
// Claude session, not the workspace. A workspace is a Linux user and a home
// directory — it cannot be "stopped", it exists until you delete it. Rendering the
// raw word next to a workspace name says something untrue.
func TestStatusIsLabelledAsTheClaudeSession(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(js, "function sessionLabel(") {
		t.Fatal("no sessionLabel(): the raw status must not be shown as the workspace's own")
	}
	for _, want := range []string{`"Claude running"`, `"Claude stopped"`} {
		if !strings.Contains(js, want) {
			t.Errorf("sessionLabel should say %s", want)
		}
	}

	// Every place that puts the status in front of a person must go through it.
	raw := regexp.MustCompile(`\$\{w?s?\.status\}|"\s*·\s*"\s*\+\s*\w+\.status`)
	if raw.MatchString(js) {
		t.Error("a raw status is being rendered next to a workspace name; use sessionLabel()")
	}
}
