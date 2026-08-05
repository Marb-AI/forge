package forge

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Marb-AI/forge/config"
)

func threeHosts() map[string]*config.Host {
	return map[string]*config.Host{
		"a": {Addr: "10.0.0.1", User: "root"},
		"b": {Addr: "10.0.0.2", User: "root"},
		"c": {Addr: "10.0.0.3", User: "root"},
	}
}

func allOf(hosts map[string]*config.Host) map[string]bool {
	want := map[string]bool{}
	for alias := range hosts {
		want[alias] = true
	}
	return want
}

// The whole point: the servers are asked at the same time, so a sweep costs what
// the slowest of them costs rather than the sum.
//
// Proved with a barrier rather than a stopwatch. A wall-clock threshold is a
// guess about a loaded machine — it passes on a quiet one and fails on a busy
// one without either saying anything about the code — whereas this cannot pass
// unless every host was already being asked before any of them was allowed to
// answer, which is the property itself.
func TestEveryHostIsAskedAtOnce(t *testing.T) {
	hosts := threeHosts()

	started := make(chan struct{}, len(hosts))
	release := make(chan struct{})
	together := make(chan struct{})

	// Nobody may answer until everybody has been asked. Asked one after another,
	// the first would wait here for a second that never comes — so the watcher
	// gives up after a while and lets it finish, and the test fails on the fact
	// rather than hanging until the package times out.
	go func() {
		for range hosts {
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				return
			}
		}
		close(together)
		close(release)
	}()

	got := askHosts(hosts, allOf(hosts), func(h *config.Host) (string, error) {
		started <- struct{}{}
		<-release
		return h.Addr, nil
	})

	select {
	case <-together:
	default:
		t.Error("the hosts were asked one after another: the first was answering " +
			"before the last had been asked, which is the cost this exists to remove")
	}
	if len(got) != len(hosts) {
		t.Errorf("got %d answers, want %d: %v", len(got), len(hosts), got)
	}
}

// A server that is off contributes nothing and stops nothing. One unreachable
// machine emptying the panel of every other is worse than a stale figure for the
// one that is down.
func TestAHostThatDoesNotAnswerIsSimplyAbsent(t *testing.T) {
	hosts := threeHosts()

	got := askHosts(hosts, allOf(hosts), func(h *config.Host) (string, error) {
		if h.Addr == "10.0.0.2" {
			return "", errors.New("connection refused")
		}
		return h.Addr, nil
	})

	if _, there := got["b"]; there {
		t.Error("a host that failed contributed an answer")
	}
	for _, alias := range []string{"a", "c"} {
		if got[alias] == "" {
			t.Errorf("host %q is missing, though it answered", alias)
		}
	}
}

// A config naming a host it no longer has is treated as unreachable, not as a
// crash: the two are the same thing from where the caller stands, and a nil
// dereference here would take the whole sweep with it.
func TestAHostTheConfigNoLongerHasIsSkipped(t *testing.T) {
	hosts := threeHosts()
	want := allOf(hosts)
	want["ghost"] = true

	var asked int32
	var mu sync.Mutex
	got := askHosts(hosts, want, func(h *config.Host) (string, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		return h.Addr, nil
	})

	if len(got) != 3 {
		t.Errorf("got %d answers for 3 real hosts and one ghost: %v", len(got), got)
	}
	if asked != 3 {
		t.Errorf("asked %d hosts, want 3", asked)
	}
}

// The answers are written from every goroutine at once, so the map that collects
// them has to be locked. Run under -race this is the test that says so.
func TestCollectingTheAnswersIsSafe(t *testing.T) {
	hosts := map[string]*config.Host{}
	for _, alias := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		hosts[alias] = &config.Host{Addr: alias, User: "root"}
	}
	got := askHosts(hosts, allOf(hosts), func(h *config.Host) (int, error) {
		return len(h.Addr), nil
	})
	if len(got) != len(hosts) {
		t.Errorf("got %d answers, want %d", len(got), len(hosts))
	}
}

// Only the servers that have something to say are asked. A host with no
// workspaces on it has nothing to answer about them, and asking it is an SSH
// round trip spent on nothing.
func TestOnlyHostsWithWorkspacesAreAsked(t *testing.T) {
	cfg := &config.Config{
		Hosts: map[string]*config.Host{
			"a": {Addr: "10.0.0.1"}, "b": {Addr: "10.0.0.2"}, "idle": {Addr: "10.0.0.9"},
		},
		Workspaces: map[string]string{"one": "a", "two": "a", "three": "b"},
	}

	want := hostsWithWorkspaces(cfg)
	if want["idle"] {
		t.Error("a host with no workspaces would be asked about workspaces")
	}
	for _, alias := range []string{"a", "b"} {
		if !want[alias] {
			t.Errorf("host %q has workspaces and would not be asked", alias)
		}
	}
	// And each of them once, however many workspaces it holds: "a" has two.
	if len(want) != 2 {
		t.Errorf("would ask %d hosts, want 2: %v", len(want), want)
	}
}
