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
// Where the list comes from depends on the host, and the three answers are not
// interchangeable — see mergeWorkspaceStatus, which is where that is decided.
//
// A host that keeps its own record of which accounts are Forge's answers for
// itself, and this device believes it: that is what lets a machine which has
// never heard of a workspace see it, which is every device but the one that
// created it. A host that keeps none — every server whose agent predates the
// record — leaves this config as the answer, as it has always been. A host that
// did not answer says nothing, which is not the same as saying no.
//
// What is never listed either way is somebody else's. The host's directory holds
// every account under /home/workspaces, including ones Forge never made, and no
// operation here will touch one: listing them would only offer what this would
// then decline to do.
func ListWorkspaces() ([]WorkspaceInfo, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	// Every host at once, and every registered one — not only those this config
	// has workspaces on. That is new, and it is the point: a host that keeps its
	// own record knows about workspaces this device has never heard of, which is
	// exactly what a second device is. Skipping it would mean a phone could only
	// ever see what the laptop had already told it.
	//
	// Unreachable hosts are absent from the result, and whatever this config knows
	// about them is reported as unreachable below.
	answers := askHosts(cfg.Hosts, everyHost(cfg),
		func(_ string, host *config.Host) (hostWorkspaces, error) {
			var res agentproto.ListResult
			if err := callAgent(host, &res, "workspace-list"); err != nil {
				return hostWorkspaces{}, err
			}
			a := hostWorkspaces{
				sessions: map[string]string{},
				ours:     map[string]bool{},
				recorded: res.Recorded,
			}
			for _, ws := range res.Workspaces {
				a.sessions[ws.Name] = ws.Status
				if ws.Ours {
					a.ours[ws.Name] = true
				}
			}
			return a, nil
		})

	out := mergeWorkspaceStatus(cfg.Workspaces, answers)
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

// WorkspaceActivity asks each host once — and all of them at the same time —
// for the Claude attention state of the workspaces on it, and keeps only the
// ones this client owns (same rule as ListWorkspaces — the host's directory may
// hold workspaces that aren't ours). A host we can't reach simply contributes
// nothing.
func WorkspaceActivity() (map[string]Activity, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	answers := askHosts(cfg.Hosts, hostsWithWorkspaces(cfg),
		func(_ string, host *config.Host) (agentproto.ActivityResult, error) {
			var res agentproto.ActivityResult
			err := callAgent(host, &res, "workspace-activity") // unreachable: its tabs stay dim
			return res, err
		})
	out := map[string]Activity{}
	for alias, res := range answers {
		for name, a := range res.Activity {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = Activity{State: a.State, TS: a.TS, Topic: a.Topic, TopicTS: a.TopicTS}
			}
		}
	}
	return out, nil
}

// WorkspaceTrack asks each host once, and all of them at once, for the session
// tracking of the workspaces on it — when the current Claude session began and
// how long the user has been present at it — and keeps only the ones this
// client owns. Same host fan-out and ownership filter as WorkspaceActivity; an
// unreachable host contributes nothing.
func WorkspaceTrack() (map[string]Track, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	answers := askHosts(cfg.Hosts, hostsWithWorkspaces(cfg),
		func(_ string, host *config.Host) (agentproto.TrackResult, error) {
			var res agentproto.TrackResult
			err := callAgent(host, &res, "workspace-track") // unreachable: clocks don't move this round
			return res, err
		})
	out := map[string]Track{}
	for alias, res := range answers {
		for name, t := range res.Sessions {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = Track{SessionStart: t.SessionStart, ActiveSeconds: t.ActiveSeconds}
			}
		}
	}
	return out, nil
}

// WorkspaceUsage asks each host once, and all of them at once, for the Claude
// usage of the workspaces on it — the login each is signed in as, its context
// and cost, and where that login stands against its rate limits — and keeps
// only the ones this client owns. Same host fan-out and ownership filter as
// WorkspaceActivity.
//
// The hosts are asked at once, like every other sweep — see fanout.go for what
// that is worth and what it deliberately is not. A host that cannot be reached
// contributes nothing and its logins keep the reading they last gave, which is
// why the sample carries its own timestamp.
//
// The rate-limit windows stay pointers through the conversion: a nil window is
// a login that has not reported one, and flattening that to a zeroed struct
// would show a full allowance where we have no reading at all.
func WorkspaceUsage() (map[string]Usage, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	answers := askHosts(cfg.Hosts, hostsWithWorkspaces(cfg),
		func(_ string, host *config.Host) (agentproto.UsageResult, error) {
			var res agentproto.UsageResult
			err := callAgent(host, &res, "workspace-usage") // unreachable, or an agent too old for the op
			return res, err
		})
	out := map[string]Usage{}
	for alias, res := range answers {
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

// mergeWorkspaceStatus is the decision, separated from the SSH so it can be
// tested: given what this config claims (name -> host alias) and what each host
// answered, what do we show?
//
// Only ours — but "ours" is now a question the host can answer, and where it
// can, its answer wins. What that buys is a device with an empty config seeing
// the workspaces on a server it can reach, and a workspace deleted from another
// device disappearing from this one instead of being shown forever.
//
// The three answers below are the whole of this function, and confusing any two
// of them is the way it goes wrong.
func mergeWorkspaceStatus(mine map[string]string, answers map[string]hostWorkspaces) []WorkspaceInfo {
	// Whose list this is, per host. A host that keeps a record is the authority on
	// its own workspaces: it is the only answer that is the same from every
	// device, which is the entire reason the record exists. A host that keeps
	// none, or did not answer, leaves this client's own config as the answer for
	// that host — which is what it has always been, and what an old server or a
	// server that is off must keep being.
	names := map[string]string{} // workspace -> host alias
	for name, alias := range mine {
		if a, ok := answers[alias]; ok && a.recorded {
			continue // the host will say; see below
		}
		names[name] = alias
	}
	for alias, a := range answers {
		if !a.recorded {
			continue
		}
		for name := range a.ours {
			names[name] = alias
		}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	out := make([]WorkspaceInfo, 0, len(sorted))
	for _, name := range sorted {
		alias := names[name]
		status := agentproto.StatusUnreachable
		if a, answered := answers[alias]; answered {
			// The host answered and doesn't have it: it is gone — deleted from another
			// machine, most likely. Reporting "stopped" would be a lie you could act
			// on (there is nothing left to start).
			status = agentproto.StatusMissing
			if s, ok := a.sessions[name]; ok {
				status = s
			}
		}
		out = append(out, WorkspaceInfo{Name: name, Host: alias, Status: status})
	}
	return out
}

// hostWorkspaces is one host's answer: the session status of everything it has,
// which of those it says are Forge's, and whether it keeps that record at all.
//
// The last one is what makes the second safe to read. An agent from before the
// record claims nothing, which is identical on the wire to a host that keeps one
// and owns none — and reading those alike would look at an old server and hide
// every workspace on it.
type hostWorkspaces struct {
	sessions map[string]string
	ours     map[string]bool
	recorded bool
}

// AdoptWorkspaces tells a host which of its accounts are Forge's, from what this
// client already believes, and reports how many it named.
//
// The migration, and the only way a record can be filled in for workspaces older
// than it: the host cannot tell its own accounts apart, so the device that made
// them has to say. Idempotent, so running it twice is running it once, and safe
// on a host that already keeps a record — it can only ever add names this client
// was already acting on.
//
// alias empty means every host this client has workspaces on. A host too old to
// know the command is reported rather than skipped silently: it is the one thing
// standing between that server and a second device, and it wants `host update`.
func AdoptWorkspaces(alias string) (map[string]Adopted, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	want := hostsWithWorkspaces(cfg)
	if alias != "" {
		if cfg.Hosts[alias] == nil {
			return nil, fmt.Errorf("no such host %q (see: forge host list)", alias)
		}
		if !want[alias] {
			return map[string]Adopted{}, nil // known host, nothing of ours on it
		}
		want = map[string]bool{alias: true}
	}

	// Sequential per host and parallel across them, like every other sweep: the
	// names on one host go into one file, and the agent's lock would serialise
	// them anyway.
	//
	// The host is also told its port range here, from this client's own. It is the
	// same migration and the same moment — everything this device knows about that
	// machine which the machine does not yet know itself — and a range set before
	// any second device exists is a range the second device will agree with.
	byName := map[string][]string{}
	for name, a := range cfg.Workspaces {
		byName[a] = append(byName[a], name)
	}
	// The callback never fails, so every host is in the answer with its own
	// account of how it went. askHosts drops failures, which is right for the
	// sweeps that feed a panel and wrong here: "it did not work" is the result.
	return askHosts(cfg.Hosts, want, func(alias string, host *config.Host) (Adopted, error) {
		var r Adopted
		for _, name := range byName[alias] {
			if err := callAgent(host, nil, "workspace-adopt", "-name", name); err != nil {
				r.Err = err
				return r, nil
			}
			r.Named++
		}
		if err := tellRange(host, cfg.PortRangeOr()); err != nil {
			r.Err = err
		}
		return r, nil
	}), nil
}

// tellRange gives a host its port range, once — the one it has been allocating
// from all along, so nothing moves. A host that already has one keeps it: it may
// have been set deliberately, and a block that moved would break every port
// written into a repo under the old one.
func tellRange(host *config.Host, r config.PortRange) error {
	var cur agentproto.PortRangeResult
	if err := callAgent(host, &cur, "host-port-range"); err != nil {
		return err
	}
	if cur.Set {
		return nil
	}
	return callAgent(host, nil, "host-port-range",
		"-start", strconv.Itoa(r.Start),
		"-end", strconv.Itoa(r.End),
		"-block", strconv.Itoa(r.Block))
}

// Adopted is what one host made of being told which workspaces are Forge's.
//
// Err is the whole reason this is a struct rather than a count: a host that
// could not be reached and an agent too old to know the command both name zero
// workspaces, and they want different things done about them.
type Adopted struct {
	Named int
	Err   error
}

// TooOld reports whether this failure is an agent that predates the command,
// rather than a server that could not be reached.
//
// It reads the agent's own words, which is a weak seam and the only one there
// is: the answer arrives as text. A miss costs the hint, not the error — the
// caller prints what went wrong either way.
func (a Adopted) TooOld() bool {
	return a.Err != nil && strings.Contains(a.Err.Error(), "unknown op")
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
