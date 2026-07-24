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
	"sync"
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

// Prompt is one saved piece of text you send Claude over and over — a title to
// recognise it by in the list, and the content that actually gets typed into the
// session.
//
// It lives in the client's config, not on a host and not per workspace, because
// that is what it is: how one PERSON works. The same "review this branch the way
// I like it" belongs in every workspace on every server you own, and a prompt
// tied to a codebase would have to be written out again for the next one.
type Prompt struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// Config is the whole client state. Forwards maps host alias -> workspace name
// -> the list of local ports to keep tunnelled, as discovered by
// `forge forwarding start`.
type Config struct {
	Hosts map[string]*Host            `json:"hosts"`
	Ports map[string]map[string][]int `json:"forwards"`
	// Prompts are the saved texts the UI's prompts panel offers, in the order they
	// are shown. A slice, not a map: the order is the user's, and a map would
	// reshuffle the list on every save.
	Prompts []Prompt `json:"prompts,omitempty"`
	// Workspaces maps a workspace name to the host alias it lives on — and it is
	// the record of which workspaces are OURS.
	//
	// The host's own list is every directory under /home/workspaces, including ones
	// Forge never created: a colleague's, or one made by hand. Those are not ours to
	// show or to touch, and every command here refuses them anyway ("not created by
	// this client"). So the list of workspaces comes from here; the host is asked
	// only for what this file cannot know — whether a Claude session is running in
	// one.
	Workspaces map[string]string `json:"workspaces"`
	// UIPort is the localhost port the browser UI (`forge ui`) binds to. Zero
	// means "unset" — callers fall back to DefaultUIPort.
	UIPort int `json:"ui_port,omitempty"`
}

// DefaultUIPort is the localhost port `forge ui` uses when none is configured.
// Deliberately obscure so it rarely collides with a dev server.
const DefaultUIPort = 47615

// UIPortOr returns the configured UI port, or DefaultUIPort if unset.
func (c *Config) UIPortOr() int {
	if c.UIPort > 0 {
		return c.UIPort
	}
	return DefaultUIPort
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

// updateMu serialises read-modify-write cycles on the config file. Save() alone
// cannot: every mutation is load, change, save, and it is the gap between the
// load and the save that loses data — two of them interleaved each read the same
// file and the second save silently drops the first one's change. Load and Save
// stay lock-free (a plain read never loses anything, and Save is atomic); the
// lock belongs to the cycle, which is what Update is.
//
// This covers one process, which is the one that matters: the UI daemon runs
// every mutation the browser can reach — a prompt saved in one tab, the UI port
// in another, a workspace being created in a third. A `forge` command in a
// terminal is a SEPARATE process and this does not serialise against it; that
// would need a lock in the filesystem, and the cost of losing that race is a
// re-typed setting, not a broken file (Save is write-temp-then-rename).
var updateMu sync.Mutex

// Update applies a change to the config as one atomic step: it loads the current
// file, hands it to change, and saves the result — with no other Update able to
// interleave.
//
// Keep change SHORT. It runs under the lock, so anything slow in it (an SSH
// round trip, say) blocks every other config write for as long as it takes. The
// pattern is: do the slow work first, then Update to record the result.
func Update(change func(*Config) error) error {
	updateMu.Lock()
	defer updateMu.Unlock()

	c, err := Load()
	if err != nil {
		return err
	}
	if err := change(c); err != nil {
		return err
	}
	return c.Save()
}

// Save writes the config atomically (write temp + rename) so a crash mid-write
// can't corrupt it.
//
// The temp file gets a unique name. A fixed one (config.json.tmp) is shared
// state between writers, and Update's lock does not reach the `forge` command
// you have open in a terminal: two processes saving at the same moment would
// write the same temp path, and the loser's rename fails with "no such file"
// after the winner already renamed it away. Losing that race should cost you the
// last writer's version of the file — not an error on a save that had nothing
// wrong with it.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Same directory as the target: rename is only atomic within a filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename below has moved it
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
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
