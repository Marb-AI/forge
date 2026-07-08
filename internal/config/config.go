// Package config manages Forge's local client state: registered hosts and the
// set of ports to keep forwarded. It lives entirely on the laptop as a single
// JSON file at ~/.forge/config.json. Workspaces themselves live on the server;
// the client only needs to know which hosts exist and what to tunnel.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Host is a registered remote server. SSH is the only entry point; User is the
// admin account used to invoke forge-agent (privileged lifecycle operations),
// while individual workspaces are reached as their own Linux users at the same
// address.
type Host struct {
	Alias string `json:"alias"`
	User  string `json:"user"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
}

// Config is the whole client state. Forwards maps host alias -> workspace name
// -> the list of local ports to keep tunnelled, as discovered by
// `forge forwarding start`.
type Config struct {
	Hosts map[string]*Host            `json:"hosts"`
	Ports map[string]map[string][]int `json:"forwards"`
	// Workspaces maps a workspace name to the host alias it lives on. This is
	// client-side convenience so `workspace <name> ssh` etc. need no host arg;
	// the server remains the source of truth for what actually exists.
	Workspaces map[string]string `json:"workspaces"`
}

// Dir returns ~/.forge, creating it if necessary.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config, returning an empty (initialised) config if none exists.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{
			Hosts:      map[string]*Host{},
			Ports:      map[string]map[string][]int{},
			Workspaces: map[string]string{},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Hosts == nil {
		c.Hosts = map[string]*Host{}
	}
	if c.Ports == nil {
		c.Ports = map[string]map[string][]int{}
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]string{}
	}
	return c, nil
}

// Save writes the config atomically (write temp + rename) so a crash mid-write
// can't corrupt it.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ParseSSHTarget splits "user@host[:port]" (or "host") into its parts, applying
// sensible defaults (user "root", port 22).
func ParseSSHTarget(s string) (user, addr string, port int, err error) {
	user, port = "root", 22
	rest := strings.TrimSpace(s)
	if rest == "" {
		return "", "", 0, fmt.Errorf("empty ssh target")
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		user = rest[:at]
		rest = rest[at+1:]
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		p, perr := strconv.Atoi(rest[colon+1:])
		if perr != nil {
			return "", "", 0, fmt.Errorf("invalid port in %q: %w", s, perr)
		}
		port = p
		rest = rest[:colon]
	}
	if rest == "" {
		return "", "", 0, fmt.Errorf("missing host in %q", s)
	}
	return user, rest, port, nil
}

// AddWorkspace records that a workspace lives on a host.
func (c *Config) AddWorkspace(name, host string) {
	if c.Workspaces == nil {
		c.Workspaces = map[string]string{}
	}
	c.Workspaces[name] = host
}

// RemoveWorkspace forgets a workspace and any forwards recorded for it.
func (c *Config) RemoveWorkspace(name string) {
	host := c.Workspaces[name]
	delete(c.Workspaces, name)
	if host != "" && c.Ports[host] != nil {
		delete(c.Ports[host], name)
		if len(c.Ports[host]) == 0 {
			delete(c.Ports, host)
		}
	}
}

// HostFor returns the host a workspace lives on, or nil if unknown.
func (c *Config) HostFor(name string) *Host {
	alias, ok := c.Workspaces[name]
	if !ok {
		return nil
	}
	return c.Hosts[alias]
}

// SetPorts records the discovered ports for a workspace on a host.
func (c *Config) SetPorts(host, workspace string, ports []int) {
	if c.Ports[host] == nil {
		c.Ports[host] = map[string][]int{}
	}
	if len(ports) == 0 {
		delete(c.Ports[host], workspace)
		if len(c.Ports[host]) == 0 {
			delete(c.Ports, host)
		}
		return
	}
	c.Ports[host][workspace] = ports
}
