package forge

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

// WorkspaceInfo is one workspace of ours, and the state of the Claude session in
// it. The status is the session's — a workspace is a Linux user and a home
// directory, and cannot itself be "stopped".
type WorkspaceInfo struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// HostUser is the host's own login account (config's Host.User) — the user
	// `host prepare` connected as: root, or a passwordless-sudo user. It names the
	// identity of the host shell, which differs per server, so a front end can label
	// it truthfully instead of assuming "root". Resolved from the config already
	// loaded here, so callers don't reload it.
	HostUser string `json:"host_user"`
	Status   string `json:"status"`
}

// Activity is what a workspace's Claude is up to: its attention state ("busy"/
// "idle"/"waiting") with the unix second it was set, and the topic Claude last
// wrote for the workspace with the unix second it wrote it.
type Activity struct {
	State   string `json:"state"`
	TS      int64  `json:"ts"`
	Topic   string `json:"topic,omitempty"`
	TopicTS int64  `json:"topic_ts,omitempty"`
}

// Track is a workspace's session tracking: SessionStart is the unix second the
// current Claude session began (held across a checkpoint, reset on stop/restart)
// and ActiveSeconds how long the user has been present at it.
type Track struct {
	SessionStart  int64 `json:"session_start"`
	ActiveSeconds int64 `json:"active_seconds"`
}

// RateWindow is one of a Claude subscription's rate-limit windows: how much of it
// is spent and when it starts over. A nil window in Usage means absent, which is not
// the same as 0% — see Usage.Auth for the case where it is absent by nature.
type RateWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
}

// Account identifies the Claude login a workspace is signed in as. UUID is the key
// to group by — one group per login, since that is the unit a rate limit is
// measured against — and the rest are labels for the group's header. An empty UUID
// means no Claude.ai login: an API-credit workspace has none.
type Account struct {
	UUID  string `json:"uuid"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Org   string `json:"org,omitempty"`
}

// Usage is one workspace's Claude usage: the login it runs as, how full its context
// window is, what its session has cost, and that login's rate-limit windows.
//
// The rate-limit windows belong to the login, not the workspace — several workspaces
// on one login report the same figures, because they spend the same allowance — so a
// display groups by Account.UUID and takes the freshest sample in each group rather
// than adding anything up. What IS per workspace, and therefore per row: the context
// window, the cost, the model.
//
// TS says when the sample was taken. Nothing here refreshes while a workspace's
// Claude is not running, so a group's figures are as old as its liveliest member and
// the caller says so rather than presenting them as current.
type Usage struct {
	Account Account `json:"account"`
	// Auth is how the workspace pays: "subscription", "api", "bedrock", "vertex", or
	// empty when nothing on the host says. It decides what the row can claim — only a
	// Claude.ai subscription HAS a 5-hour or weekly window, so for anything else their
	// absence is the nature of the thing and not a gap in our reading.
	Auth        string      `json:"auth,omitempty"`
	TS          int64       `json:"ts,omitempty"`
	Model       string      `json:"model,omitempty"`
	ContextUsed int64       `json:"context_used,omitempty"`
	ContextSize int64       `json:"context_size,omitempty"`
	CostUSD     float64     `json:"cost_usd,omitempty"`
	FiveHour    *RateWindow `json:"five_hour,omitempty"`
	SevenDay    *RateWindow `json:"seven_day,omitempty"`
	// Note is the few words saying why a workspace's figures are missing or frozen
	// when the reason is knowable — a higher-precedence Claude Code setting owning
	// the status line Forge collects through. Empty when there is nothing to explain.
	Note string `json:"note,omitempty"`
}

// ListWorkspaces returns the workspaces THIS CLIENT created, with the state of the
// Claude session in each.
//
// The list comes from our config, not from the host. The host's own list is every
// directory under /home/workspaces — including ones Forge never made, belonging to
// someone else or created by hand. Those are not ours: every operation refuses to
// touch a workspace that isn't in the config ("not created by this client"), so
// listing them only offers what we will then decline to do.
//
// The host is still asked, but only for what the config cannot know: whether a
// Claude session is running. That answer costs an SSH round trip, which is why the
// name and host — the parts we already have — are never made to wait for it.
func ListWorkspaces() ([]WorkspaceInfo, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	// Ask only the hosts we actually have workspaces on, and each of them once. A
	// host we have nothing on has nothing to tell us, and every one of these is an
	// SSH round trip.
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}

	sessions := map[string]map[string]string{} // host alias -> workspace -> session status
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue // config names a host it no longer has; treated as unreachable
		}
		var res agentproto.ListResult
		if err := callAgent(host, &res, "workspace-list"); err != nil {
			continue // unreachable; its workspaces are reported as such below
		}
		byName := map[string]string{}
		for _, ws := range res.Workspaces {
			byName[ws.Name] = ws.Status
		}
		sessions[alias] = byName
	}

	out := mergeWorkspaceStatus(cfg.Workspaces, sessions)
	// Fill in each workspace's host login user from the config already in hand —
	// mergeWorkspaceStatus works off the name→alias map alone (so it stays unit-
	// testable), and only here do we still hold cfg.Hosts to resolve the user.
	for i := range out {
		if h := cfg.Hosts[out[i].Host]; h != nil {
			out[i].HostUser = h.User
		}
	}
	return out, nil
}

// WorkspaceActivity asks each host once for the Claude attention state of the
// workspaces on it, and keeps only the ones this client owns (same rule as
// ListWorkspaces — the host's directory may hold workspaces that aren't ours). A
// host we can't reach simply contributes nothing.
func WorkspaceActivity() (map[string]Activity, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}
	out := map[string]Activity{}
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue
		}
		var res agentproto.ActivityResult
		if err := callAgent(host, &res, "workspace-activity"); err != nil {
			continue // unreachable: its tabs just stay dim
		}
		for name, a := range res.Activity {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = Activity{State: a.State, TS: a.TS, Topic: a.Topic, TopicTS: a.TopicTS}
			}
		}
	}
	return out, nil
}

// WorkspaceTrack asks each host once for the session tracking of the workspaces on
// it — when the current Claude session began and how long the user has been present
// at it — and keeps only the ones this client owns. Same host fan-out and ownership
// filter as WorkspaceActivity; an unreachable host contributes nothing.
func WorkspaceTrack() (map[string]Track, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}
	out := map[string]Track{}
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue
		}
		var res agentproto.TrackResult
		if err := callAgent(host, &res, "workspace-track"); err != nil {
			continue // unreachable: its clocks just don't update this round
		}
		for name, t := range res.Sessions {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = Track{SessionStart: t.SessionStart, ActiveSeconds: t.ActiveSeconds}
			}
		}
	}
	return out, nil
}

// WorkspaceUsage asks each host once for the Claude usage of the workspaces on it —
// the login each is signed in as, its context and cost, and where that login stands
// against its rate limits — and keeps only the ones this client owns. Same host
// fan-out and ownership filter as WorkspaceActivity.
//
// The hosts are asked one after another, like the other two sweeps and unlike
// HostStats: those fan out because the servers panel asks every registered machine
// including the empty ones, while this only ever visits hosts we actually keep
// workspaces on. A host that cannot be reached contributes nothing and its logins
// keep the reading they last gave, which is why the sample carries its own timestamp.
//
// The rate-limit windows stay pointers through the conversion: a nil window is a
// login that has not reported one, and flattening that to a zeroed struct would show
// a full allowance where we have no reading at all.
func WorkspaceUsage() (map[string]Usage, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}
	out := map[string]Usage{}
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue
		}
		var res agentproto.UsageResult
		if err := callAgent(host, &res, "workspace-usage"); err != nil {
			continue // unreachable, or an agent too old to know the op
		}
		for name, u := range res.Usage {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = usage(u)
			}
		}
	}
	return out, nil
}

// usage converts one workspace's reading from the agent's wire type.
func usage(u agentproto.Usage) Usage {
	window := func(w *agentproto.RateWindow) *RateWindow {
		if w == nil {
			return nil
		}
		return &RateWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
	}
	return Usage{
		Account: Account{
			UUID: u.Account.UUID, Email: u.Account.Email,
			Name: u.Account.Name, Org: u.Account.Org,
		},
		Auth:        u.Auth,
		TS:          u.TS,
		Model:       u.Model,
		ContextUsed: u.ContextUsed,
		ContextSize: u.ContextSize,
		CostUSD:     u.CostUSD,
		FiveHour:    window(u.FiveHour),
		SevenDay:    window(u.SevenDay),
		Note:        u.Note,
	}
}

// mergeWorkspaceStatus is the decision, separated from the SSH so it can be tested:
// given the workspaces our config claims (name -> host alias) and what each host
// reported (alias -> name -> session status), what do we show?
//
// Only ours. A workspace the host has but our config doesn't is somebody else's, or
// was made by hand — and every operation refuses to touch it anyway.
func mergeWorkspaceStatus(mine map[string]string, sessions map[string]map[string]string) []WorkspaceInfo {
	names := make([]string, 0, len(mine))
	for name := range mine {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]WorkspaceInfo, 0, len(names))
	for _, name := range names {
		alias := mine[name]
		status := agentproto.StatusUnreachable
		if byName, answered := sessions[alias]; answered {
			// The host answered and doesn't have it: it is gone — deleted from another
			// machine, most likely. Reporting "stopped" would be a lie you could act
			// on (there is nothing left to start).
			status = agentproto.StatusMissing
			if s, ok := byName[name]; ok {
				status = s
			}
		}
		out = append(out, WorkspaceInfo{Name: name, Host: alias, Status: status})
	}
	return out
}

// CreateWorkspace provisions a workspace on a registered host and records it
// locally.
//
// It returns the port block the workspace was given, which is the one thing about
// a new workspace the caller has to be told: it is what the ports in its repo will
// have to be.
func CreateWorkspace(name, alias string) (*PortBlock, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	host := cfg.Hosts[alias]
	if host == nil {
		return nil, fmt.Errorf("no such host %q (see: forge host list)", alias)
	}

	pubkey, err := workspaceKeys()
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(pubkey)

	// Before creating anything: allocation reads every host, and a failure here
	// should leave nothing behind. A workspace created and then found to have no
	// block would need cleaning up by hand.
	//
	// The block is reserved for the duration, because what follows is slow —
	// creating a workspace installs Claude Code from the network — and until it
	// lands, nothing else asking for a block would see this one taken.
	block, err := allocateBlock(cfg, name, alias)
	if err != nil {
		return nil, err
	}

	var res agentproto.CreateResult
	if err := callAgent(host, &res, "workspace-create",
		"--name", name,
		"--pubkey", enc,
		"--port-start", strconv.Itoa(block.Start),
		"--port-size", strconv.Itoa(block.Size),
	); err != nil {
		releaseBlock(name)
		return nil, err
	}

	// Record it in its own step, and only it. The load above is minutes old by now
	// (creating the user on the server is an SSH round trip), so saving that whole
	// copy back would undo anything else written meanwhile — the UI port, a server
	// just registered, another workspace created from a second tab.
	if err := updateConfig(func(c *config.Config) error {
		c.AddWorkspace(name, alias)
		// The workspace now holds the block on its host, which is the real record;
		// the reservation has done its job. Dropped in the same update that records
		// the workspace, so there is no moment where neither speaks for the block.
		c.ReleasePortBlock(name)
		return nil
	}); err != nil {
		return nil, err
	}
	return block, nil
}

// DeleteWorkspace destroys a workspace on its host and forgets it locally.
// This is irreversible: the agent runs `userdel -r`, so the workspace's Linux
// user and its entire home — every file in it — are gone.
func DeleteWorkspace(name string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	host := cfg.HostFor(name)
	if host == nil {
		return fmt.Errorf("unknown workspace %q — not created by this client", name)
	}
	if err := callAgent(host, nil, "workspace-delete", "--name", name); err != nil {
		return err
	}
	return updateConfig(func(c *config.Config) error {
		c.RemoveWorkspace(name)
		return nil
	})
}

// TrackInc adds seconds of user-present time to a workspace's session tracking, via
// the agent on its host. The browser flushes accumulated activity here; a flush that
// can't reach the host simply doesn't land and the next one carries the arrears.
func TrackInc(name string, seconds int) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	host := cfg.HostFor(name)
	if host == nil {
		return fmt.Errorf("unknown workspace %q", name)
	}
	return callAgent(host, nil, "workspace-track-inc",
		"-name", name, "-seconds", strconv.Itoa(seconds))
}

// workspaceKeys are the public keys a new workspace lets in.
//
// This device's, always. It is the key Forge connects with, so a workspace that
// did not have it would be one Forge could create and never open again — and
// after the transport flips there is no second key to fall back on.
//
// FORGE_PUBKEY adds one BESIDE it, rather than replacing it as it used to. The
// escape hatch is worth keeping: somebody may want to reach the workspace from a
// plain terminal, without Forge in the middle. Replacing was the wrong shape for
// it, because a single environment variable could then produce a workspace this
// client cannot enter, and nothing would say so until the first attempt.
//
// authorized_keys is a list, so this costs nothing but a second line.
func workspaceKeys() ([]byte, error) {
	k, err := Keys()
	if err != nil {
		return nil, err
	}
	mine, err := k.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%w (run: forge setup)", err)
	}
	lines := []string{strings.TrimSpace(mine)}

	if p := os.Getenv("FORGE_PUBKEY"); p != "" {
		extra, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("FORGE_PUBKEY=%q: %w", p, err)
		}
		if line := strings.TrimSpace(string(extra)); line != "" {
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
