package ui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rate-limit window belongs to an ACCOUNT. Three workspaces signed into one login
// are drawing down the same five-hour allowance, so the panel groups by the login
// and shows one figure per group — the freshest report of it. Adding them up, or
// averaging them, would invent a number that is nowhere true.
func TestLoginPanelGroupsByAccountAndDoesNotSumWindows(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	body := jsFunc(t, js, "loginGroups")

	if !strings.Contains(body, "account.uuid") {
		t.Error("groups must key on the account uuid: the same person in two organisations is two allowances")
	}
	// The freshest sample speaks for the group. Anything accumulating percentages
	// across members is the bug this test exists for.
	if !strings.Contains(body, "> g.ts") {
		t.Error("a group should take its windows from the newest sample, not the first or the last")
	}
	if regexp.MustCompile(`(five|seven)[^\n]*\+=`).MatchString(body) {
		t.Error("rate-limit windows must never be accumulated across a group")
	}
	// A group carries its members' names for the tooltip, not a row each — the panel
	// is one line per login.
	if !strings.Contains(body, "g.names.push(") {
		t.Error("a group should collect its workspace names for the tooltip")
	}
}

// The panel is ordered by name, and by nothing that moves. It used to lead with
// the login closest to a limit, which sounds right and reads badly: those figures
// change while you work, so the rows swap under you and every glance costs a
// re-read. Urgency is carried by the figure and by the colour it turns, neither of
// which moves a row.
func TestTheClaudePanelIsOrderedByName(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "loginGroups")

	// The groups' own sort, named exactly: this function sorts twice, and the other
	// one puts a group's workspace names in order for the tooltip. A looser match
	// finds that one first and passes while the panel reorders itself.
	sortCall := regexp.MustCompile(`\[\.\.\.groups\.values\(\)\]\.sort\(([^;]*)\);`).FindStringSubmatch(body)
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

// The context window is a property of one session, so it belongs with the other
// per-workspace facts at the top of the pane and nowhere else. A context figure in
// the login panel would be a number about nothing: several workspaces on one login
// have several different contexts.
func TestContextIsShownPerWorkspaceOnly(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	index := embeddedAsset(t, "index.html")

	if !strings.Contains(index, `id="ws-ctx"`) {
		t.Error("index.html has no #ws-ctx — the per-workspace context has nowhere to render")
	}
	if !strings.Contains(jsFunc(t, js, "renderIdent"), "contextPercent(") {
		t.Error("renderIdent should render the context percentage")
	}
	if strings.Contains(jsFunc(t, js, "loginGroupRow"), "contextPercent(") {
		t.Error("the login panel must not show a context figure — it is per session, not per login")
	}
}

// Money was deliberately taken out of this panel: nothing actionable follows from
// it, and on a subscription the figure isn't even a bill. This test exists so it
// doesn't drift back in.
func TestLoginPanelShowsNoCosts(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	for _, fn := range []string{"loginGroupRow", "windowSpan", "loginTitle", "renderIdent"} {
		body := jsFunc(t, js, fn)
		if strings.Contains(body, "cost_usd") {
			t.Errorf("%s() reads cost_usd — the panel deliberately shows no costs", fn)
		}
	}
	// And no formatter survives to be called from anywhere else.
	if strings.Contains(js, "function fmtCost(") {
		t.Error("app.js still carries a currency formatter")
	}
}

// Everything in this panel came out of a file in a workspace home — the login email
// and the model name are written by Claude Code, into a directory whose Linux user
// can put anything there. Same hazard as the topic, same rule: textContent, never
// innerHTML.
func TestLoginPanelRendersTextNotMarkup(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	for _, fn := range []string{"loginGroupRow", "windowSpan", "renderIdent"} {
		body := jsFunc(t, js, fn)
		if strings.Contains(body, ".innerHTML") {
			t.Errorf("%s() writes innerHTML; a login label is untrusted text", fn)
		}
		if !strings.Contains(body, ".textContent") {
			t.Errorf("%s() paints nothing with textContent — check it still renders", fn)
		}
	}
	// replaceChildren with built nodes, not a string of HTML assembled from labels.
	if !strings.Contains(jsFunc(t, js, "renderLogins"), "replaceChildren(") {
		t.Error("renderLogins should replace children with built nodes")
	}
}

// Absent is not zero, all the way to the last pixel. An organisation on API credits
// has no five-hour window at all, and two figures reading 0% would tell them they
// have plenty of an allowance that does not exist — so such a group says it has no
// windows instead of showing numbers.
func TestLoginPanelDoesNotDrawBarsForWindowsThatDoNotExist(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	body := jsFunc(t, js, "loginGroupRow")

	if !strings.Contains(body, "if (g.five || g.seven)") {
		t.Error("the percentages must be conditional on a window existing")
	}
	if !strings.Contains(body, "lgn-nowin") {
		t.Error("a group with no windows should say so rather than showing two figures")
	}
	// A window the login HAS but that wasn't in the last sample reads as 0%, the way
	// Claude's own usage display puts it — the distinction that matters is the one
	// above: no windows at all is not zero.
	if body := jsFunc(t, js, "windowSpan"); !strings.Contains(body, "w ? Math.max(") {
		t.Error("an unreported window should read as 0%, not as a dash")
	}
	// Flat by construction: no meters, so three logins cost three lines.
	if strings.Contains(body, "meterRow(") {
		t.Error("the login panel should not draw meters — it is one line per login")
	}
}

// Each round is an SSH round trip per host, so the poll is gated the way the
// servers one is: parked when nothing can be seen, re-armed after each poll settles
// rather than on a fixed timer (an ssh to a hung host outlasts any interval).
func TestUsagePollParksItselfButStillFillsTheLoginChip(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	for _, want := range []string{"function usageWanted(", "document.hidden", "loginsCollapsed()"} {
		if !strings.Contains(js, want) {
			t.Errorf("the usage poll should be gated by %s", want)
		}
	}
	if regexp.MustCompile(`setInterval\([^)]*pollUsage`).MatchString(js) {
		t.Error("the usage poll must re-arm after each round settles, not run on setInterval")
	}
	// The one difference from the servers panel: identity is wanted even when the
	// panel is collapsed, or the login chip at the top of the pane would be empty
	// for everyone who keeps the meters closed.
	if !strings.Contains(jsFunc(t, js, "usageWanted"), "!usage.loaded") {
		t.Error("a collapsed panel must still allow the first load, or the login chip never fills")
	}
	if !strings.Contains(js, "usage.busy") {
		t.Error("nothing prevents a slow usage poll from overlapping the next one")
	}
}

// Same reasoning as the servers panel: polling faster than the daemon's freshness
// window buys nothing but meters that move in fits and starts.
func TestUsagePollIsNotFasterThanTheFreshnessWindow(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	m := regexp.MustCompile(`USAGE_POLL_MS\s*=\s*(\d+)`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js has no USAGE_POLL_MS: the Claude panel has no defined poll interval")
	}
	ms, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("USAGE_POLL_MS is not a number: %v", err)
	}
	if poll := time.Duration(ms) * time.Millisecond; poll < statsFresh {
		t.Errorf("app.js polls usage every %v but the daemon reuses answers for %v", poll, statsFresh)
	}
}

// The layout the panels were asked for: identification at the top of the left pane
// (what this workspace is doing, whose login it spends, which server it runs on),
// metrics at the bottom (Claude's limits, then the machines).
func TestPaneKeepsIdentityAtTheTopAndMetricsAtTheBottom(t *testing.T) {
	index := embeddedAsset(t, "index.html")

	for _, id := range []string{"ws-ident", "ws-login", "ws-server", "logins", "loginlist", "logins-count"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("index.html has no #%s", id)
		}
	}
	topic := strings.Index(index, `id="ws-topic"`)
	ident := strings.Index(index, `id="ws-ident"`)
	tree := strings.Index(index, `id="filetree"`)
	ports := strings.Index(index, `id="ports"`)
	logins := strings.Index(index, `id="logins"`)
	servers := strings.Index(index, `id="servers"`)
	if !(topic < ident && ident < tree) {
		t.Errorf("the login/server chips belong under the topic and above the tree (%d, %d, %d)",
			topic, ident, tree)
	}
	// The pane is in two halves, and the seam is what a tab switch replaces. Above
	// it: the tree and the ports, both answers about the workspace you have open.
	// Below it: the Claude allowances and the servers, which every session shares
	// and which therefore must not move when you change tabs.
	if !(tree < ports && ports < logins && logins < servers) {
		t.Errorf("the pane's order is tree, ports, Claude, servers — what changes with "+
			"the tab on top, what every session shares underneath (%d, %d, %d, %d)",
			tree, ports, logins, servers)
	}
}

// A login with no rate-limit windows shows one prose label where two figures would
// go. The row is a grid whose window columns are 46px wide, so that label has to
// span them — squeezed into one it wraps or overflows, which is how the panel
// stops being three lines for three logins.
func TestLoginWithNoWindowsSpansTheWindowColumns(t *testing.T) {
	css := embeddedAsset(t, "app.css")
	block := regexp.MustCompile(`(?s)\.lgn-nowin\s*\{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("app.css has no .lgn-nowin rule")
	}
	if !strings.Contains(block, "grid-column: 2 / -1") {
		t.Errorf(".lgn-nowin must span the window columns, got:\n%s", block)
	}
	// The row is a grid; a flex declaration on its children does nothing but
	// mislead whoever maintains it next.
	for _, child := range []string{`.lgn-name`, `.lgn-nowin`} {
		b := regexp.MustCompile(`(?s)\` + child + `\s*\{[^}]*\}`).FindString(css)
		if strings.Contains(b, "flex:") {
			t.Errorf("%s carries a flex declaration, but .lgn is a grid:\n%s", child, b)
		}
	}
}
