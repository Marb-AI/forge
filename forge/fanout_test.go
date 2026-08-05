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
// The test measures rather than counts goroutines, because the property is about
// elapsed time and nothing else — three hosts that each take 200ms must finish in
// nearer 200 than 600.
func TestEveryHostIsAskedAtOnce(t *testing.T) {
	hosts := threeHosts()
	const each = 200 * time.Millisecond

	start := time.Now()
	got := askHosts(hosts, allOf(hosts), func(h *config.Host) (string, error) {
		time.Sleep(each)
		return h.Addr, nil
	})
	elapsed := time.Since(start)

	if len(got) != 3 {
		t.Fatalf("got %d answers, want 3: %v", len(got), got)
	}
	// Halfway between "at once" and "one after another" is the only threshold
	// that says something on a loaded machine.
	if elapsed > 3*each/2 {
		t.Errorf("three hosts taking %v each took %v — they were asked one after "+
			"another, which is the cost this exists to remove", each, elapsed)
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
