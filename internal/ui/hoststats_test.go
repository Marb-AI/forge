package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostStatsEndpointReturnsRows(t *testing.T) {
	s, h := testServer(t)
	s.deps.HostStats = func() ([]HostStat, error) {
		return []HostStat{{
			Host: "remote-dev-01", Addr: "1.2.3.4", Reachable: true,
			CPUPercent: 12.5, CPUCores: 4,
			MemTotal: 8 << 30, MemUsed: 3 << 30,
			DiskPath: "/home/workspaces", DiskTotal: 80 << 30, DiskUsed: 20 << 30,
			Uptime: 3600,
		}}, nil
	}
	rec := authorized(h, "/api/hosts/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []HostStat
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if len(got) != 1 || got[0].Host != "remote-dev-01" || got[0].CPUCores != 4 || got[0].MemUsed != 3<<30 {
		t.Errorf("stats not passed through: %+v", got)
	}
	// Polled endpoint: a cached copy would leave the meters showing a dead reading.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// The dep is optional, like WorkspaceActivity: a caller that doesn't wire it must
// get an empty panel, not a broken one.
func TestHostStatsEndpointNilDep(t *testing.T) {
	_, h := testServer(t)
	rec := authorized(h, "/api/hosts/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("nil HostStats should 200 empty, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "[]\n" {
		t.Errorf("body = %q, want an empty list", body)
	}
}

// Every open tab polls this panel on its own timer, and answering costs an SSH
// round trip PER REGISTERED HOST. Tabs that ask at the same moment must therefore
// share one measurement, and a tab that asks again within the freshness window
// must be handed the last one — otherwise leaving the UI open in a few tabs
// multiplies the load on every server you own.
func TestHostStatsIsSharedAndReusedWithinTheWindow(t *testing.T) {
	const askers = 10

	var calls atomic.Int32
	release := make(chan struct{})
	joined := make(chan struct{}, askers)
	clock := time.Unix(0, 0)
	s := &server{
		now:         func() time.Time { return clock },
		onStatsJoin: func() { joined <- struct{}{} },
		deps: Deps{HostStats: func() ([]HostStat, error) {
			calls.Add(1)
			<-release // hold the leader open so the others pile up behind it
			return []HostStat{{Host: "h1", Reachable: true, CPUCores: 2}}, nil
		}},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	got := make([][]HostStat, askers)
	for i := range askers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stats, err := s.hostStatsShared()
			if err != nil {
				t.Errorf("asker %d: %v", i, err)
			}
			got[i] = stats
		}()
	}
	close(start)
	waitJoins(t, joined, askers-1, 2*time.Second)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("%d tabs asking at once produced %d SSH fan-outs, want 1", askers, n)
	}
	for i, stats := range got {
		if len(stats) != 1 || stats[0].Host != "h1" {
			t.Errorf("asker %d got %v, want the shared result", i, stats)
		}
	}

	// Inside the window: reused, no new fan-out.
	release = make(chan struct{})
	close(release)
	clock = clock.Add(statsFresh - time.Second)
	if _, err := s.hostStatsShared(); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("an ask inside the freshness window made %d calls, want 1 (reuse)", n)
	}

	// Past it: measured again, or the panel would show a frozen reading.
	clock = clock.Add(2 * time.Second)
	if _, err := s.hostStatsShared(); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("an ask past the freshness window made %d calls, want 2 (re-measured)", n)
	}
}

// A failed measurement must not be held: caching it would go on reporting a
// server as down for the rest of the window after it had already come back.
func TestHostStatsFailureIsNotReused(t *testing.T) {
	var calls atomic.Int32
	clock := time.Unix(0, 0)
	s := &server{
		now: func() time.Time { return clock },
		deps: Deps{HostStats: func() ([]HostStat, error) {
			calls.Add(1)
			return nil, errors.New("no config")
		}},
	}
	for i := range 2 {
		if _, err := s.hostStatsShared(); err == nil {
			t.Fatalf("ask %d: want the error through", i)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a cached failure: %d calls for 2 asks, want 2", n)
	}
}

// The browser's poll interval and the daemon's freshness window are two halves of
// one decision, written in two languages. If the poll ever became the faster of
// the two, a single tab would be served the previous round's numbers half the
// time — meters that update in visible fits and starts, for no saving at all.
func TestBrowserPollIsNotFasterThanTheFreshnessWindow(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	m := regexp.MustCompile(`SERVERS_POLL_MS\s*=\s*(\d+)`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js has no SERVERS_POLL_MS: the servers panel has no defined poll interval")
	}
	ms, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("SERVERS_POLL_MS is not a number: %v", err)
	}
	if poll := time.Duration(ms) * time.Millisecond; poll < statsFresh {
		t.Errorf("app.js polls every %v but the daemon reuses answers for %v — a lone tab would be shown stale readings",
			poll, statsFresh)
	}
}

// The panel must not turn a background tab into a permanent load on every server:
// its loop is parked while nothing can be seen, and re-armed after each poll
// settles rather than on a fixed timer (an ssh to a hung host outlasts the
// interval, so a repeating timer would stack connections on it).
func TestServersPanelParksItsPollWhenUnseen(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	for _, want := range []string{"function serversWanted(", "document.hidden", "serversCollapsed()"} {
		if !strings.Contains(js, want) {
			t.Errorf("the servers poll should be gated by %s", want)
		}
	}
	if regexp.MustCompile(`setInterval\([^)]*[Ss]ervers`).MatchString(js) {
		t.Error("the servers poll must re-arm after each attempt settles, not run on setInterval")
	}
	if !strings.Contains(js, "servers.busy") {
		t.Error("nothing prevents a slow poll from overlapping the next one")
	}
}
