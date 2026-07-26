// Package config manages Forge's local client state: registered hosts and the
// set of ports to keep forwarded. It lives entirely on the laptop as a single
// JSON file at ~/.forge/config.json. Workspaces themselves live on the server;
// the client only needs to know which hosts exist and what to tunnel.
//
// It sits outside internal/ because the core does: a Host is what several of the
// core's operations take and return, so anything that can call them has to be able
// to name the type.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// PortRange is the span of host ports Forge may hand out, and how big a block
	// each workspace gets. Zero values mean "unset" — see PortRangeOr.
	PortRange PortRange `json:"port_range,omitempty"`
	// PortReservations are blocks promised to workspaces that are still being
	// created. See PortReservation.
	PortReservations []PortReservation `json:"port_reservations,omitempty"`
}

// PortReservation is a block handed to a workspace that does not exist yet.
//
// It closes a window that is much wider than it looks. A block becomes visible to
// the next allocation only once the workspace is on its host and answers
// `workspace-list` — and creating one installs Claude Code over the network, so
// that is minutes, not milliseconds. Two creations started in that window (the UI
// daemon and a terminal, or two terminals) would each read the same "lowest free"
// block and hand it out twice, breaking the one guarantee the whole scheme rests
// on, and surfacing much later as a tunnel that cannot bind.
//
// Writing the reservation is a single atomic config update, so the two creations
// see each other immediately rather than minutes apart.
type PortReservation struct {
	Workspace string `json:"workspace"`
	Host      string `json:"host"`
	Start     int    `json:"start"`
	// At is the unix second the block was promised, so a reservation left behind by
	// a creation that died can be ignored rather than blocking that block forever.
	At int64 `json:"at"`
}

// ReservationTTL is how long a reservation counts for. Generous, because the thing
// it covers is slow: a workspace creation installs Claude Code from the network and
// a slow host can take minutes. The cost of being too generous is one unusable
// block for half an hour; the cost of being too eager is the double-allocation this
// exists to prevent.
const ReservationTTL = 30 * time.Minute

// ReservePortBlock promises a block to a workspace about to be created, replacing
// any reservation that workspace already had (a retried creation reserves again).
func (c *Config) ReservePortBlock(workspace, host string, start int, now time.Time) {
	c.ReleasePortBlock(workspace)
	c.PortReservations = append(c.PortReservations, PortReservation{
		Workspace: workspace, Host: host, Start: start, At: now.Unix(),
	})
}

// ReleasePortBlock forgets a workspace's reservation — called once the workspace
// exists (its block is now on the host, which is the real record) and also when
// creating it failed.
func (c *Config) ReleasePortBlock(workspace string) {
	kept := c.PortReservations[:0]
	for _, r := range c.PortReservations {
		if r.Workspace != workspace {
			kept = append(kept, r)
		}
	}
	c.PortReservations = kept
}

// ActiveReservations returns the reservations still worth honouring, dropping any
// whose creation must have died. Expiry is what keeps a crashed `workspace create`
// from stranding a block permanently.
func (c *Config) ActiveReservations(now time.Time) []PortReservation {
	cutoff := now.Add(-ReservationTTL).Unix()
	var live []PortReservation
	for _, r := range c.PortReservations {
		if r.At >= cutoff {
			live = append(live, r)
		}
	}
	return live
}

// PortRange is the territory Forge allocates from: every workspace on every host
// this client knows gets one immutable Block of it, and publishes only inside that
// block.
//
// It lives in the CLIENT's config, not on a host, because it is the laptop that
// suffers a collision. A workspace's remote port doubles as its local port, so the
// range has to be unique across every server at once — and the client is the only
// party that sees them all.
//
// Nothing depends on the range being remembered, which is why it is not copied to
// the hosts: allocation avoids the blocks that actually exist (read back from every
// host), not the ones the range predicts. A reinstalled laptop that falls back to
// the default therefore still cannot collide — it would only start handing out
// blocks from a different part of the space than the user originally picked.
//
// It is chosen once and deliberately generous: the port space costs nothing, and a
// range wide enough that nobody ever runs out of blocks is what keeps block size
// from being a decision anyone has to make.
type PortRange struct {
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	// Block is how many ports each workspace gets. Uniform across the range, which
	// is what makes a port readable: with blocks of 100 from 16000, 16104 is the
	// fifth port of the second workspace. Per-workspace sizes would turn allocation
	// into a packing problem with holes and make that arithmetic impossible.
	Block int `json:"block,omitempty"`
}

// Defaults for PortRange. 16000–30000 in blocks of 100 is 140 workspaces, which is
// far more than anyone will have — the range is free, so it is sized to make
// "I ran out of blocks" a case that never happens rather than one to handle.
//
// High enough that nothing else is there: the bottom of the ephemeral range starts
// at 32768 on Linux, and dev servers cluster in the low thousands (3000, 5173,
// 8080), so this sits in the quiet gap between them.
const (
	DefaultPortStart = 16000
	DefaultPortEnd   = 30000
	DefaultPortBlock = 100
)

// PortRangeOr returns the configured range with any unset field defaulted, so
// callers never have to check. A partially configured range is completed rather
// than rejected: `forge ports range` can set the span without restating the block.
func (c *Config) PortRangeOr() PortRange {
	r := c.PortRange
	if r.Start <= 0 {
		r.Start = DefaultPortStart
	}
	if r.End <= 0 {
		r.End = DefaultPortEnd
	}
	if r.Block <= 0 {
		r.Block = DefaultPortBlock
	}
	return r
}

// Blocks returns every block position in the range, lowest first. The range is cut
// into fixed-size blocks from Start; a tail too short for a whole block is not a
// block, because a workspace with fewer ports than its neighbours would be a
// surprise nobody asked for.
func (r PortRange) Blocks() []int {
	// A zero or negative block would step the loop below by nothing and never
	// terminate. Callers are meant to come through PortRangeOr, which cannot
	// produce one — but this is an exported method on an exported type, and a
	// method whose contract is "call something else first or the process hangs"
	// is a trap rather than an API.
	if r.Block <= 0 || r.Start <= 0 {
		return nil
	}
	var starts []int
	for p := r.Start; p+r.Block-1 <= r.End; p += r.Block {
		starts = append(starts, p)
	}
	return starts
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
// every mutation the browser can reach — the UI port set in one tab, a workspace
// being created in another. A `forge` command in a terminal is a SEPARATE process
// and this does not serialise against it; that would need a lock in the
// filesystem, and the cost of losing that race is a re-typed setting, not a
// broken file (Save is write-temp-then-rename).
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
