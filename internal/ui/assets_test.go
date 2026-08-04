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
	re := regexp.MustCompile(`(?ms)^(?:async )?function ` + regexp.QuoteMeta(name) + `\(.*?^\}`)
	body := re.FindString(js)
	if body == "" {
		t.Fatalf("app.js has no top-level function %s()", name)
	}
	return body
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
}

// The topic is the one string in this UI that a language model composes and the
// browser then displays. It arrives as free text through two hops that do not
// escape anything (a file in a home directory, then JSON), so the only thing
// standing between a topic containing markup and that markup running is this: it
// is written with textContent, never innerHTML.
func TestTopicIsRenderedAsTextNotMarkup(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	index := embeddedAsset(t, "index.html")

	for _, id := range []string{"ws-topic", "ws-topic-text", "ws-topic-age"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("index.html has no #%s — renderTopic would paint nothing", id)
		}
	}
	if !strings.Contains(js, `getElementById("ws-topic-text").textContent`) {
		t.Error("the topic is not written with textContent — model-authored markup would be live in the DOM")
	}
	if regexp.MustCompile(`ws-topic[^)]*\)\.innerHTML`).MatchString(js) {
		t.Error("the topic is written with innerHTML somewhere; it must be textContent")
	}

	// The topic pane sits above the clocks and the tree: it answers the first
	// question you have on a tab you last touched days ago, so it must not be below
	// the fold of a long file list.
	topic := strings.Index(index, `id="ws-topic"`)
	track := strings.Index(index, `id="track-banner"`)
	tree := strings.Index(index, `id="filetree"`)
	if topic < 0 || track < 0 || tree < 0 || topic > track || topic > tree {
		t.Errorf("the topic pane must come before the tracking banner and the file tree (%d, %d, %d)",
			topic, track, tree)
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

// paddingOf finds the padding a selector is given in the stylesheet, the way
// zIndexOf finds a z-index: every rule the selector appears in, last one wins,
// and a selector grouped with others counts. Anchoring a regex to the selector
// and taking the first match would read a rule the cascade has already
// overridden, and would miss the selector entirely once it is listed beside
// another — neither of which is a difference this test should be blind to.
func paddingOf(t *testing.T, css, selector string) string {
	t.Helper()
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")

	rule := regexp.MustCompile(`(?s)([^{}]*)\{([^}]*)\}`)
	pad := regexp.MustCompile(`padding:\s*([^;]+);`)

	found := ""
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
		if pm := pad.FindStringSubmatch(body); pm != nil {
			found = strings.TrimSpace(pm[1])
		}
	}
	if found == "" {
		t.Fatalf("%s sets no padding", selector)
	}
	return found
}

// The topic and the identity under it are two halves of one corner of the pane,
// and they only read as one if they sit in the same box. The identity's top
// padding was 0, which put its first line against the rule above it — and, for a
// workspace with no topic yet, against the top edge of the pane itself.
func TestTheIdentityBlockSitsInTheSameBoxAsTheTopic(t *testing.T) {
	css := embeddedAsset(t, "app.css")
	topic, ident := paddingOf(t, css, "#ws-topic"), paddingOf(t, css, "#ws-ident")
	if topic != ident {
		t.Errorf("#ws-topic has padding %q and #ws-ident %q — the two stack, so a "+
			"difference shows as one of them hugging the rule between them", topic, ident)
	}
}

// The helper above reads a stylesheet, so it gets one of its own: what it must
// survive is the cascade (a later rule wins), a selector listed beside another,
// and a comment sitting where a rule would be.
func TestPaddingOfReadsTheCascade(t *testing.T) {
	const css = `
#a { padding: 1px; }
/* #a { padding: 999px; } — a rule in a comment is not a rule */
#a-suffix { padding: 3px; }
#a[hidden] { display: none; }
#b, #a { padding: 2px 4px; }
`
	if got := paddingOf(t, css, "#a"); got != "2px 4px" {
		t.Errorf("padding of #a = %q, want the last rule that names it (%q)", got, "2px 4px")
	}
	if got := paddingOf(t, css, "#b"); got != "2px 4px" {
		t.Errorf("padding of #b = %q — a selector grouped with another still has one", got)
	}
	if got := paddingOf(t, css, "#a-suffix"); got != "3px" {
		t.Errorf("padding of #a-suffix = %q; a longer name is not the shorter one", got)
	}
}

// The eye over the clocks hides the TIMES, not the banner: tracking keeps running,
// the controls stay where they are, and the same button brings the numbers back.
// It used to hide the whole strip — which took the buttons with it, including the
// only way to undo it, and turned "I don't want to watch the counter" into "the
// feature is gone".
//
// Checked the way the design describes it: the file tree's class flip, applied to
// the clocks. And what the banner itself does must not depend on that setting.
func TestTheEyeHidesTheTimesAndNotTheBanner(t *testing.T) {
	css, js := embeddedAsset(t, "app.css"), embeddedAsset(t, "app.js")

	flip := regexp.MustCompile(`#track-banner\.numbers-hidden\s+\.track-clocks\s*\{[^}]*display:\s*none`)
	if !flip.MatchString(css) {
		t.Error("nothing hides .track-clocks on the banner's own class — the eye has " +
			"nothing to flip, so it can only be hiding something bigger")
	}

	rule := regexp.MustCompile(`banner\.hidden\s*=\s*([^;]+);`).FindStringSubmatch(js)
	if rule == nil {
		t.Fatal("nothing decides whether the banner is shown")
	}
	if strings.Contains(rule[1], "track.hidden") {
		t.Errorf("the banner is shown by %q — the eye is for the times, and a banner "+
			"that goes with them takes its own controls out of reach", rule[1])
	}
}

// A metric is an icon and a figure, and they have to read as one thing. Pinned to
// the two edges of a fixed-width cell — an 11px/1fr grid — the space inside a pair
// grew with the cell while the space between pairs stayed at the row's own gap, so
// every figure sat nearer the NEXT metric's icon than its own.
func TestAServerMetricHoldsItsIconAndItsFigureTogether(t *testing.T) {
	css := embeddedAsset(t, "app.css")
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")

	rule := regexp.MustCompile(`\.srv-metrics \.m\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if rule == nil {
		t.Fatal("app.css has no rule for a server metric")
	}
	body := rule[1]
	if strings.Contains(body, "grid-template-columns") {
		t.Errorf("a metric is laid out as %q — columns hold the icon and the figure "+
			"apart by as much as the cell is wide", strings.TrimSpace(body))
	}
	if !strings.Contains(body, "flex-end") {
		t.Errorf("a metric is laid out as %q — without an end alignment the slack "+
			"lands between the icon and its own figure", strings.TrimSpace(body))
	}
}

// The Claude panel is ordered by name, and by nothing that moves. Leading with
// the login closest to a limit sounds right and reads badly: those figures change
// while you work, so the rows swap under you and every glance costs a re-read.
// Urgency is carried by the figure and its colour, which move nothing.
func TestTheClaudePanelIsOrderedByName(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	sortCall := regexp.MustCompile(`\[\.\.\.groups\.values\(\)\]\.sort\(([^;]*)\);`).FindStringSubmatch(js)
	if sortCall == nil {
		t.Fatal("nothing decides the order of the login groups")
	}
	if !strings.Contains(sortCall[1], "localeCompare") {
		t.Errorf("the logins are ordered by %q, which is not their names", sortCall[1])
	}
	for _, moving := range []string{"Pressure", "used_percent", "five", "seven", "ts"} {
		if strings.Contains(sortCall[1], moving) {
			t.Errorf("the logins are ordered by %q — %q changes while you work, so the "+
				"rows swap under you", sortCall[1], moving)
		}
	}
}
