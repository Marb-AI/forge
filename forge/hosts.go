package forge

import (
	"sort"

	"github.com/Marb-AI/forge/config"
)

// HostFor resolves a workspace name to the host it lives on, or nil if this client
// has no such workspace.
//
// The config is read on every call rather than held: the UI daemon runs for days,
// and a workspace created in one browser tab has to resolve in the next request
// from another.
func HostFor(name string) *config.Host {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	return cfg.HostFor(name)
}

// ListHosts returns the registered host aliases, sorted — the servers a new
// workspace can be put on.
func ListHosts() ([]string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(cfg.Hosts))
	for a := range cfg.Hosts {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return aliases, nil
}
