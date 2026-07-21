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

// The stream ends the same way whether the Claude session died or the ssh link
// carrying it did — handleTermStream writes one "end" event for both. So an end
// the browser did not ask for is not evidence the session stopped, and reporting
// "Session stopped" for a dropped connection tells you your work is gone at the
// exact moment Claude is still working. The browser has to go and ask the host.
func TestUnexpectedStreamEndIsDiagnosedNotAssumedStopped(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(js, "function diagnoseEnd(") {
		t.Fatal("no diagnoseEnd(): a dropped connection would be reported as a stopped session")
	}
	// It must reach the verdict from the host's status, not from the end event.
	if !strings.Contains(js, `"lost"`) || !strings.Contains(js, `"checking"`) {
		t.Error("diagnoseEnd should resolve an unexplained end into checking -> lost/stopped")
	}
	// A session the host still calls running must never be offered a "start": it
	// never stopped, and the button has to say so.
	if !strings.Contains(js, "Reconnect") {
		t.Error("a lost connection to a running session must offer Reconnect, not Start")
	}
	if !strings.Contains(js, `state.endCause = "checking"`) {
		t.Error("the stream's end handler must mark the cause unknown before the host answers")
	}
}

// The reattach loop runs forever, so its shape is what keeps a server outage from
// turning into a self-inflicted denial of service: no ControlMaster means every
// attempt is a full SSH handshake, and sshd refuses new ones past MaxStartups.
func TestReconnectBackoffCannotStampede(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(js, "function scheduleReconnect(") {
		t.Fatal("no scheduleReconnect(): retries would have no backoff")
	}
	// A repeating timer is the bug: an SSH connect to a hung host blocks for the
	// TCP timeout, which outlasts any sane interval, so attempts would stack.
	if regexp.MustCompile(`setInterval\([^)]*(reconnect|diagnose|reattach)`).MatchString(js) {
		t.Error("reconnect must re-arm after each attempt settles, not run on setInterval")
	}
	if !strings.Contains(js, "state.reconnect.busy") {
		t.Error("nothing prevents a slow attempt from overlapping the next one")
	}
	// The tail must be random, or every tab and every machine knocks in unison the
	// instant the server returns — precisely when MaxStartups starts refusing.
	if !strings.Contains(js, "Math.random()") {
		t.Error("the backoff tail must be jittered so tabs don't synchronise")
	}
	// A backgrounded tab is the multiplier: without this, twenty open tabs are
	// twenty loops.
	if !strings.Contains(js, "document.hidden") {
		t.Error("a hidden tab must park its loop rather than keep handshaking")
	}
	// Backoff must reset on evidence the link works, not on the decision to retry.
	if !strings.Contains(js, "sess.gotData") {
		t.Error("the backoff should reset when a byte actually arrives, not on attach")
	}
}
