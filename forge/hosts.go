package forge

import (
	"fmt"
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

// RemoveHost forgets a server locally. It does NOT touch the machine: the server
// keeps running, and so do its workspaces — Forge just stops knowing about them,
// which is why this one is reversible, with `forge host add`.
func RemoveHost(alias string) error {
	return config.Update(func(c *config.Config) error {
		if _, ok := c.Hosts[alias]; !ok {
			return fmt.Errorf("no such host %q", alias)
		}
		delete(c.Hosts, alias)
		delete(c.Ports, alias)
		for ws, host := range c.Workspaces {
			if host == alias {
				delete(c.Workspaces, ws)
			}
		}
		return nil
	})
}
