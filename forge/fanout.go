package forge

import (
	"sync"

	"github.com/Marb-AI/forge/config"
)

// Asking every server at once.
//
// Each of these questions — what is running, what is Claude doing, what has it
// cost, how full is the disk — is one SSH round trip per host, and they were
// asked one after another. The cost of that is not the sum of the work but the
// sum of the waiting: a handshake is most of each trip, and four servers meant
// four handshakes end to end before the first pixel changed. It is the whole of
// what a second machine costs you, and on a phone's connection it is the whole
// of the startup.
//
// So they are asked together. Nothing else changes: a host that does not answer
// still contributes nothing, which is what keeps one unreachable server from
// emptying the screen of the others.
//
// HostStats already did this, with a comment saying why — "with a fan-out, the
// slowest host costs what the slowest host costs and no more". It keeps its own,
// because it differs in two ways that matter: its rows are ordered, and a failure
// is part of its answer rather than an absence. What is here is that same idea
// for the four sweeps that had not had it.
//
// What this deliberately is not: a persistent client per host. That is the other
// half of the same cost — the handshake happens on every sweep, not only the
// first — and it needs a lifetime and a reconnect policy, which is a decision of
// its own and wants measuring rather than guessing.

// askHosts puts the same question to every named host at the same time and
// returns what came back, keyed by alias.
//
// A host that fails is absent from the result rather than an error: every caller
// here treats an unreachable server as one that said nothing this round, because
// the alternative — one server down, the whole panel empty — is worse than stale
// figures for the machine that is off.
//
// The answers are collected under a lock rather than through a channel because
// the shape of the result is a map: with one writer per key and the caller
// waiting for all of them, a mutex is the smaller thing to read.
func askHosts[T any](hosts map[string]*config.Host, want map[string]bool,
	ask func(*config.Host) (T, error)) map[string]T {

	out := map[string]T{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for alias := range want {
		host := hosts[alias]
		if host == nil {
			// The config names a host it no longer has. Treated as unreachable,
			// which is what it is.
			continue
		}
		wg.Add(1)
		go func(alias string, host *config.Host) {
			defer wg.Done()
			v, err := ask(host)
			if err != nil {
				return
			}
			mu.Lock()
			out[alias] = v
			mu.Unlock()
		}(alias, host)
	}
	wg.Wait()
	return out
}

// hostsWithWorkspaces is the set of servers worth asking anything about
// workspaces: the ones this client has some on.
//
// A host with none has nothing to say, and every question is an SSH round trip —
// so the set is computed rather than the map of hosts being walked.
func hostsWithWorkspaces(cfg *config.Config) map[string]bool {
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}
	return needed
}
