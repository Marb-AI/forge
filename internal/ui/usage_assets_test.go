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
	// Cost and context ARE per workspace and stay on their own rows.
	if !strings.Contains(body, "g.rows.push(") {
		t.Error("each workspace should keep its own row inside the group")
	}
}

// Everything in this panel came out of a file in a workspace home — the login email
// and the model name are written by Claude Code, into a directory whose Linux user
// can put anything there. Same hazard as the topic, same rule: textContent, never
// innerHTML.
func TestLoginPanelRendersTextNotMarkup(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	for _, fn := range []string{"loginGroupRow", "loginWorkspaceRow", "renderIdent"} {
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
// has no five-hour window at all, and two bars sitting at 0% would tell them they
// have plenty of an allowance that does not exist — so a group with no windows shows
// what it does have (spend) instead of bars.
func TestLoginPanelDoesNotDrawBarsForWindowsThatDoNotExist(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	body := jsFunc(t, js, "loginGroupRow")

	if !strings.Contains(body, "if (g.five || g.seven)") {
		t.Error("the bars must be conditional on a window existing")
	}
	if !strings.Contains(body, "lgn-spend") {
		t.Error("a group with no windows should show its spend rather than empty bars")
	}
	// A window that exists but was not reported this round is a different thing
	// again, and the meter's own "—" says so: null, not 0.
	if !strings.Contains(body, "g.five ? g.five.used_percent : null") {
		t.Error("a missing window must reach meterRow as null, so it renders as — and not as 0%")
	}
}

// The panel exists to be glanced at, so the login about to stop working belongs at
// the top. Sorted by whichever of its windows is fullest.
func TestLoginGroupsAreOrderedByPressure(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	groups := jsFunc(t, js, "loginGroups")
	if !strings.Contains(groups, "groupPressure(b) - groupPressure(a)") {
		t.Error("groups should be ordered fullest-window first")
	}
	// A group with no window must not sort as 0% — that would put it above a login
	// at 3% while claiming to know something about it. It goes last, hence -1.
	if !strings.Contains(jsFunc(t, js, "groupPressure"), "-1") {
		t.Error("a group with no window should sort below every group that has one")
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
	logins := strings.Index(index, `id="logins"`)
	servers := strings.Index(index, `id="servers"`)
	if !(topic < ident && ident < tree) {
		t.Errorf("the login/server chips belong under the topic and above the tree (%d, %d, %d)",
			topic, ident, tree)
	}
	if !(tree < logins && logins < servers) {
		t.Errorf("the metrics panels belong below the tree, Claude above the servers (%d, %d, %d)",
			tree, logins, servers)
	}
}
