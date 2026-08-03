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

// Keeping the agent in step with the client that talks to it.
//
// The agent is not a package anybody installs: it rides inside this binary and
// gets onto a server by being uploaded. That was `host prepare`'s job while
// preparing a server was something you did once — but the two halves are one
// release, they speak a vocabulary that grows, and a client newer than the agent
// it is talking to is not a degraded Forge, it is an unreliable one: some
// operations answer and some come back "unknown op" depending on which release
// added them.
//
// So updating the agent is its own operation, and deliberately a small one. It
// does not provision: no package manager, no docker, no firewall, no keys, none
// of the choices `host prepare` exists to make. It reads what the far end is
// running, and when that is not this build, it puts this build there. That is
// the whole of it, which is what makes it safe to run on a live server and cheap
// enough to run on every install.
//
// It needs the same access `host prepare` does — root, or an admin with
// passwordless sudo — because the binary lands in /usr/local/bin. The sudoers
// rule Forge installs covers running the agent, not replacing it.

// agentPath is where the agent lives on a server. Kept in step with the
// provisioning script, which puts it there.
const agentPath = "/usr/local/bin/forge-agent"

// agentUpload is where the new binary lands before it is moved into place.
const agentUpload = "/tmp/forge-agent.new"

// AgentUpdate is what happened to one host.
type AgentUpdate struct {
	Host string
	// Was is the build the host was running, empty when it was too old to say
	// (the version verb arrived in v0.10.0) or had no agent at all.
	Was string
	// Now is what it runs after this, read back from the host rather than assumed.
	Now string
	// Changed is false for a host that was already running this build — the
	// common case, and the reason this is cheap to run routinely.
	Changed bool
	// Err is this host's failure, held rather than returned so that one
	// unreachable server does not hide the others.
	Err error
}

// UpdateAgent puts this build's agent on one host.
func UpdateAgent(alias string) (AgentUpdate, error) {
	cfg, err := loadConfig()
	if err != nil {
		return AgentUpdate{}, err
	}
	h := cfg.Hosts[alias]
	if h == nil {
		return AgentUpdate{}, fmt.Errorf("no such host %q (see: forge host list)", alias)
	}
	return updateAgentOn(h), nil
}

// UpdateAgents does the same for every registered host, in a stable order.
//
// It never fails as a whole: a host that is off, or behind a route that is down,
// is one entry with an error in it. The others are still brought into step, and
// the caller decides what an entry with an error means — which for the installer
// that calls this is "say so and carry on", because a server being down is not a
// reason for an install to fail.
func UpdateAgents() ([]AgentUpdate, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	aliases := make([]string, 0, len(cfg.Hosts))
	for alias, h := range cfg.Hosts {
		if h != nil {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)

	out := make([]AgentUpdate, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, updateAgentOn(cfg.Hosts[alias]))
	}
	return out, nil
}

// updateAgentOn is the whole of it, for one host.
func updateAgentOn(h *config.Host) AgentUpdate {
	u := AgentUpdate{Host: h.Alias, Now: version.String()}

	// Ask first. A host already running this build is the common case once the
	// installer does this on every upgrade, and skipping it turns "make sure they
	// agree" into something you can run without thinking about it.
	//
	// A failure here is not one: an agent too old to have the verb, or a server
	// with none at all, is exactly what this is here to fix. Only the answer is
	// used, never the error.
	var res agentproto.VersionResult
	if err := callAgent(h, &res, "version"); err == nil {
		u.Was = res.Version
		if res.Version == u.Now {
			return u
		}
	}

	target := sshx.AdminTarget(h)
	goarch, err := probeGoarch(target)
	if err != nil {
		u.Err = err
		return u
	}
	src, _, closeSrc, err := agentReader(goarch)
	if err != nil {
		u.Err = err
		return u
	}
	defer closeSrc()

	if err := target.Pipe(src, io.Discard, io.Discard, "cat > "+agentUpload); err != nil {
		u.Err = fmt.Errorf("upload failed: %w", err)
		return u
	}
	if err := installAgent(h, target); err != nil {
		u.Err = err
		return u
	}

	// Read back what is there now rather than reporting what we sent. They are
	// the same thing when this worked, and when they are not, that is the one
	// fact worth having.
	if err := callAgent(h, &res, "version"); err == nil {
		u.Now = res.Version
	}
	u.Changed = true
	return u
}

// installAgent moves the uploaded binary into place, as root.
//
// Staged next to the target and renamed over it, rather than copied onto it: a
// binary that is executing cannot be opened for writing (ETXTBSY on Linux), and
// the agent is invoked by every poll the UI makes, so "nothing is running right
// now" is not something to rely on. A rename is atomic and leaves whatever was
// mid-flight running on the old inode until it exits.
func installAgent(h *config.Host, target sshx.Target) error {
	staging := agentPath + ".new"
	script := fmt.Sprintf("install -m 0755 %s %s && mv %s %s && rm -f %s",
		agentUpload, staging, staging, agentPath, agentUpload)

	remote := []string{"sh", "-c", "'" + script + "'"}
	if h.User != "root" {
		// Broader than the sudoers rule Forge installs, which covers running the
		// agent and not replacing it — the same access `host prepare` has always
		// needed, and the same message when it is missing.
		remote = append([]string{"sudo"}, remote...)
	}
	var out strings.Builder
	if err := target.Pipe(nil, &out, &out, remote...); err != nil {
		return fmt.Errorf("installing %s failed (needs root or passwordless sudo): %w: %s",
			agentPath, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// probeGoarch asks a host which CPU it has, as the Go name for it — the one
// thing an upload has to know, because the client carries an agent per
// architecture.
func probeGoarch(target sshx.Target) (string, error) {
	out, err := target.Output("uname -m")
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", target.User+"@"+target.Addr, err)
	}
	return unameToGoArch(strings.TrimSpace(string(out)))
}
