package forge

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
	"github.com/Marb-AI/forge/internal/version"
)

// AddHost registers a server this client has already prepared (or that was
// prepared from another machine), under alias. It only records it: nothing is
// installed and nothing on the server is touched — that is what `host prepare`
// is for.
//
// jump is the route to it, empty for a server this machine can reach directly.
// It is checked here rather than trusted: a route that cannot be read is a host
// that cannot be reached, and finding that out at the first connection means
// finding it out from a failure that mentions none of this.
func AddHost(target, alias, jump string) (*config.Host, error) {
	user, addr, port, err := config.ParseSSHTarget(target)
	if err != nil {
		return nil, err
	}
	if _, err := sshx.ParseJump(jump); err != nil {
		return nil, err
	}
	host := &config.Host{Alias: alias, User: user, Addr: addr, Port: port, Jump: jump}
	// The "already exists" check belongs inside the update, not before it: checked
	// against a copy loaded earlier, two adds of the same alias would both pass it
	// and the second would overwrite the first.
	if err := updateConfig(func(c *config.Config) error {
		if _, exists := c.Hosts[alias]; exists {
			return fmt.Errorf("host %q already exists", alias)
		}
		c.Hosts[alias] = host
		return nil
	}); err != nil {
		return nil, err
	}
	return host, nil
}

// Hosts returns every registered server, sorted by alias. ListHosts answers the
// same question with only the names, which is all the browser's wizard needs;
// this one is for showing where each of them actually is.
func Hosts() ([]*config.Host, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(cfg.Hosts))
	for a := range cfg.Hosts {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	hosts := make([]*config.Host, 0, len(aliases))
	for _, a := range aliases {
		hosts = append(hosts, cfg.Hosts[a])
	}
	return hosts, nil
}

// GhLogin authenticates gh once for a whole host, into the host's own gh config
// directory rather than the admin's home. `workspace create` then copies that
// credential into each new workspace, so you log in once per server instead of
// once per workspace — the same shape as the host's git identity.
//
// The login is interactive (a browser code, or a token on stdin), which is why
// it cannot happen during prepare and why it takes a terminal.
func GhLogin(alias string, out io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	host := cfg.Hosts[alias]
	if host == nil {
		return fmt.Errorf("no such host %q (see: forge host list)", alias)
	}
	// GH_CONFIG_DIR puts hosts.yml under the host's own directory instead of
	// ~/.config/gh. The file holds a token, so it stays root-only; the agent copies
	// it in as root at create time.
	remote := "install -d -m 0755 " + HostGhDir +
		" && GH_CONFIG_DIR=" + HostGhDir + " gh auth login" +
		" && chmod 0600 " + HostGhDir + "/hosts.yml"
	if host.User != "root" {
		remote = "sudo sh -c '" + remote + "'"
	}
	// Said here rather than by the caller, so it is said only once the host has
	// been resolved: "logging in on a host" followed by "no such host" is a poor
	// way to learn you mistyped the alias.
	fmt.Fprintf(out, "logging gh in on %s (interactive)…\n", alias)
	return interactive(out, sshx.AdminTarget(host).TTYArgs(remote))
}

// hostFor resolves a workspace name to the host it lives on, or nil if this client
// has no such workspace.
//
// The config is read on every call rather than held: the UI daemon runs for days,
// and a workspace created in one browser tab has to resolve in the next request
// from another.
//
// Unexported: which machine a workspace is on, and what it takes to log into it,
// is this package's business. A front end asks whether the workspace exists (see
// KnowsWorkspace) and then asks for an operation on it by name.
func hostFor(name string) *config.Host {
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	return cfg.HostFor(name)
}

// KnowsWorkspace reports whether this client has a workspace by that name — the
// question every per-workspace endpoint asks before doing anything, so an
// unknown name is answered as unknown rather than attempted and failed.
func KnowsWorkspace(name string) bool { return hostFor(name) != nil }

// ListHosts returns the registered host aliases, sorted — the servers a new
// workspace can be put on.
func ListHosts() ([]string, error) {
	cfg, err := loadConfig()
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
	return updateConfig(func(c *config.Config) error {
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

// Version is the build this client is, for a front end that wants to show it.
//
// It comes through the core like everything else a front end needs: the CLI is
// an adapter over these operations and names no package but this one, and the
// browser will want the same string in the same words.
func Version() string { return version.String() }

// AgentVersion asks a host which build of Forge installed the agent on it.
//
// The client carries the agent it uploads, so the answer is the version of the
// client that last prepared that server — which is the question you have when a
// feature is missing and you are trying to work out which end is behind. An
// agent too old to have this verb says so in those words, because "unknown op"
// is the answer and it is not a useful one on its own.
func AgentVersion(alias string) (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	h := cfg.Hosts[alias]
	if h == nil {
		return "", fmt.Errorf("no such host %q (see: forge host list)", alias)
	}
	var res agentproto.VersionResult
	if err := callAgent(h, &res, "version"); err != nil {
		if strings.Contains(err.Error(), "unknown op") {
			return "", fmt.Errorf("the agent on %q predates `forge-agent version` — re-run `forge host prepare` to replace it", alias)
		}
		return "", err
	}
	return res.Version, nil
}
