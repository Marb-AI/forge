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

// jsFunc returns the body of a top-level function in app.js, relying on the
// file's formatting: a declaration starts at column 0 and its closing brace is
// the next line that is a lone "}" at column 0.
func jsFunc(t *testing.T, js, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^function ` + regexp.QuoteMeta(name) + `\(.*?^\}`)
	body := re.FindString(js)
	if body == "" {
		t.Fatalf("app.js has no top-level function %s()", name)
	}
	return body
}

// Sending a prompt is two things that must arrive in one order — the text, then
// the Enter that submits it — and the input endpoint is fetch(), which has none.
// Posted separately, the Enter can land first: Claude submits an empty prompt and
// the text arrives after, into whatever it showed next.
func TestPromptIsSentAsASingleInputWrite(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	body := jsFunc(t, js, "sendPrompt")

	if n := strings.Count(body, "postInput("); n != 1 {
		t.Errorf("sendPrompt makes %d input posts; the text and its Enter must go in one, "+
			"or they can arrive out of order", n)
	}
	if !strings.Contains(body, `+ "\r"`) {
		t.Error("sendPrompt should append the submitting CR to the same payload")
	}
}

// A prompt worth saving is usually several lines. Typed as-is, every newline is
// an Enter — so a four-line prompt would be sent as four half-finished ones.
// Bracketed paste is what makes it arrive as a single message.
func TestMultiLinePromptIsSentAsOneMessage(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	body := jsFunc(t, js, "sendPrompt")

	if !strings.Contains(body, `\x1b[200~`) || !strings.Contains(body, `\x1b[201~`) {
		t.Error("sendPrompt must wrap the text in a bracketed paste, or its newlines submit it line by line")
	}
	// Only when the session actually has the mode on: wrapping a plain shell's
	// input in brackets it never enabled would type the escape codes themselves.
	if !strings.Contains(body, "bracketedPasteMode") {
		t.Error("the bracketed-paste wrapper must be conditional on the session having the mode enabled")
	}
}

// The popover hangs off the rail, and the rail sits beside the shell overlay —
// so a popover painting below that overlay would be invisible exactly when a
// shell is open. It must still stay under the modals, and under the confirm
// dialog, which is what asks before it deletes a prompt.
func TestPromptsPopoverPaintsAboveTheShellPanelAndBelowTheModals(t *testing.T) {
	css := embeddedAsset(t, "app.css")

	prompts := zIndexOf(t, css, "#prompts")
	if ssh := zIndexOf(t, css, "#sshpanel"); prompts <= ssh {
		t.Errorf("#prompts (z-index %d) must paint above #sshpanel (z-index %d)", prompts, ssh)
	}
	for _, above := range []string{"#settings", "#confirm"} {
		if z := zIndexOf(t, css, above); prompts >= z {
			t.Errorf("#prompts (z-index %d) must stay below %s (z-index %d) — %s opens over it",
				prompts, above, z, above)
		}
	}
}

// The rail is markup on one side and a switch on the other, with nothing but a
// string between them: a button whose action no case handles is a button that
// silently does nothing.
func TestEveryRailActionHasAHandler(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	index := embeddedAsset(t, "index.html")

	rail := regexp.MustCompile(`(?s)<aside id="rail">.*?</aside>`).FindString(index)
	if rail == "" {
		t.Fatal("index.html has no rail")
	}
	actions := regexp.MustCompile(`data-action="([a-z]+)"`).FindAllStringSubmatch(rail, -1)
	if len(actions) == 0 {
		t.Fatal("the rail has no buttons")
	}
	for _, m := range actions {
		if !strings.Contains(js, `case "`+m[1]+`"`) {
			t.Errorf("rail button %q has no case in the rail's click handler", m[1])
		}
	}
	// The one this test was written for.
	if !strings.Contains(rail, `data-action="prompts"`) {
		t.Error("the rail has no prompts button")
	}
}

// Prompts describe how their author works, so they belong to the person and not
// to a browser profile: kept in the daemon's config they are the same list in
// every tab, on every workspace, and on the phone pointed at the same daemon.
// localStorage would give each browser its own, and lose them all when someone
// clears site data.
func TestPromptsAreStoredByTheDaemonNotTheBrowser(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(js, `"/api/prompts"`) {
		t.Error("app.js never fetches /api/prompts — the library would not be shared or persisted")
	}
	if regexp.MustCompile(`localStorage\.(get|set)Item\("forge-prompts`).MatchString(js) {
		t.Error("prompts must not live in localStorage; they belong to the person, not to one browser")
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
