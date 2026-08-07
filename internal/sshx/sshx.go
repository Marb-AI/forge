// Package sshx is how Forge reaches a server. It is the single place that knows
// what "connect to a host" means — every other package names a Target and a
// command, and never a key, a port or an ssh option.
//
// There are two ways to reach one, behind the Backend seam in backend.go: the
// system's ssh binary, which is the default and what Forge has always done, and
// a client of our own built on golang.org/x/crypto/ssh. This file is the part
// both share (what a target is) plus the exec'd side of it: the argv, and the
// one session that is still attached to the caller's own terminal.
package sshx

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/config"
)

// connectTimeout bounds how long we wait for a server to answer at all.
//
// Without it, ssh hangs on the operating system's TCP timeout — measured at over
// 45 seconds against an unreachable address — and every command that touches that
// host hangs with it, including the browser UI's workspace list. Generous enough
// for a slow link, short enough that a dead host is reported rather than waited on.
const connectTimeout = 10

// commonOpts are applied to every connection: fail fast on a dead server rather
// than hanging on a long TCP timeout, and never prompt interactively for a
// password (Forge is key-only).
//
// ConnectTimeout is what makes the first half of that true. ServerAlive* only
// notices a peer that dies *after* the connection is up; a host that never answers
// at all is the connect timeout's problem, and for a long time nothing set one.
func commonOpts(t Target) []string {
	opts := []string{
		"-o", "ConnectTimeout=" + strconv.Itoa(connectTimeout),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		// Key-only, and now actually enforced. BatchMode=no is the default and does
		// nothing to stop password auth: `ssh -G` reported passwordauthentication yes
		// the whole time this file claimed otherwise. Turning both methods off makes a
		// bad key fail immediately and honestly ("Permission denied (publickey)")
		// rather than dropping into a prompt — which, in the UI daemon, is a prompt
		// nobody is there to answer.
		//
		// BatchMode stays "no" so that a *local* key passphrase can still be asked for.
		// That is a different thing from the server asking for a password.
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "BatchMode=no",
		// Said rather than assumed. Dropping -A stops Forge asking for agent
		// forwarding; it does not stop the user's own ~/.ssh/config turning it on
		// for this host, and a `ForwardAgent yes` there would put the agent back
		// on the far end without anything here mentioning it. The Go client cannot
		// be configured into it at all, so this is the exec backend catching up to
		// what the other one already guarantees.
		//
		// The reason is 2.2: git on the server runs under the identity host prepare
		// put there, and an agent on top of it only obscures who is pushing.
		"-o", "ForwardAgent=no",
		// TOFU: record a new server's host key on first connect instead of
		// refusing non-interactively (you own the servers Forge connects to).
		// A *changed* key still fails loudly — that's a real warning. The
		// pure-Go client follows the same policy in a file of Forge's own,
		// because this option writes to one that is OpenSSH's — knownhosts.go.
		"-o", "StrictHostKeyChecking=accept-new",
	}
	// The device key, and only it. Since the workspaces Forge makes admit that key
	// and nothing else, an ssh that offered ~/.ssh instead would be turned away
	// from a workspace this very client had just created — and IdentitiesOnly is
	// what stops it offering them anyway, since -i only ADDS to the list ssh
	// would otherwise try.
	//
	// Skipped when this device keeps its key somewhere a path cannot describe;
	// there is no ssh binary on such a device either.
	if key := identityFile(); key != "" {
		opts = append(opts, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	// -J, and the same string the pure-Go client parses: the login of every hop
	// is already spelled out in it (see jumpChain), so ssh cannot fall back to
	// this machine's username on one client while the other uses the host's.
	if t.Jump != "" {
		opts = append(opts, "-J", t.Jump)
	}
	if t.Port != 0 && t.Port != 22 {
		opts = append(opts, "-p", strconv.Itoa(t.Port))
	}
	return opts
}

// Target is a resolved SSH destination.
type Target struct {
	User string // login user (admin for agent ops, or the workspace name)
	Addr string
	Port int
	// Jump is the servers this one is reached through, nearest first, in ssh's -J
	// syntax — see config.Host.Jump. Every hop in it names its login: what the
	// user typed is completed when the target is built, so both clients read the
	// same route.
	Jump string
}

func (t Target) dest() string { return t.User + "@" + t.Addr }

// via is how a hop is named when something goes wrong on the way: the login and
// the address, port and all. A jump is often on a port that is not 22, and
// "permission denied" against the wrong end of a route is an hour spent looking
// in the wrong place.
func (t Target) via() string { return t.User + "@" + t.addr() }

// addr is the destination as a dialler wants it, with ssh's default port filled
// in.
func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Addr, strconv.Itoa(port))
}

// AdminTarget is the host's admin account (used to invoke forge-agent).
func AdminTarget(h *config.Host) Target {
	return Target{User: h.User, Addr: h.Addr, Port: h.Port, Jump: jumpChain(h)}
}

// WorkspaceTarget reaches a workspace as its own Linux user at the host address.
func WorkspaceTarget(h *config.Host, workspace string) Target {
	return Target{User: workspace, Addr: h.Addr, Port: h.Port, Jump: jumpChain(h)}
}

// jumpChain is the host's route with every hop's login filled in.
//
// A hop written without one takes the host's own login rather than this
// machine's username, which is what `ssh -J bastion` would have used. Two
// reasons, and the second is the one that matters: a workspace target logs in as
// the workspace, a name no bastion has ever heard of, so the target's own user
// is no answer either; and a login left implicit is a login the two clients
// would fill in differently — ssh from the local username, the Go client from
// its own default — which is the one thing a second transport must never do.
func jumpChain(h *config.Host) string {
	spec := strings.TrimSpace(h.Jump)
	if spec == "" {
		return ""
	}
	hops := strings.Split(spec, ",")
	for i, hop := range hops {
		hop = strings.TrimSpace(hop)
		if hop != "" && !strings.Contains(hop, "@") {
			hop = h.User + "@" + hop
		}
		hops[i] = hop
	}
	return strings.Join(hops, ",")
}

// ParseJump splits a route into the hops it names, nearest first. It is what
// makes a jump written by hand an error at the moment it is written rather than
// the first time something tries to connect — see forge.AddHost.
func ParseJump(spec string) ([]Target, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var hops []Target
	for _, s := range strings.Split(spec, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("jump %q: empty hop", spec)
		}
		user, addr, port, err := config.ParseSSHTarget(s)
		if err != nil {
			return nil, fmt.Errorf("jump %q: %w", spec, err)
		}
		hops = append(hops, Target{User: user, Addr: addr, Port: port})
	}
	return hops, nil
}

// Args returns the ssh argv for a non-interactive remote command (no TTY).
//
// The exec'd backend's own business — an operation asks for Output or Pipe and
// never sees an argv, because there is no argv when the pure-Go client runs it.
func (t Target) Args(remote ...string) []string {
	args := commonOpts(t)
	args = append(args, t.dest())
	args = append(args, remote...)
	return args
}

// TTYArgs returns the ssh argv for an interactive command (-t forces a TTY),
// used for shells and tmux attach.
//
// Still exported for the sessions that hand over *this* process's terminal — the
// CLI's `workspace ssh`, `claude attach`, `host shell`, which go through
// RunInteractiveTo rather than the Backend seam. A terminal opened for a front
// end does not come through here any more: it asks the target to Open a Shell,
// and only the exec'd backend turns that into an argv.
func (t Target) TTYArgs(remote ...string) []string {
	args := append([]string{"-t"}, commonOpts(t)...)
	args = append(args, t.dest())
	args = append(args, remote...)
	return args
}

// ttyArgs is the argv for a Shell: the interactive argv and nothing else.
//
// It used to carry -A as well, lending your agent to the far end so that git in
// a workspace shell used your keys. That went with 2.2 — the workspace has a git
// identity of its own, put there when it was created, and forwarding an agent on
// top of it meant the same push was signed by different keys depending on
// whether a person or Claude ran it.
func (t Target) ttyArgs(s Shell) []string {
	return t.TTYArgs(s.Remote...)
}

// LocalForwardArgs returns the ssh argv for a single local port forward with no
// remote command (-N).
//
// The supervisor's tunnels no longer come through here — they ask the target to
// Forward a port, and only the exec'd backend turns that into an argv. What is
// left is `forge expose`, the one tunnel held in the CLI's own foreground, for
// the same reason its interactive sessions are (see RunInteractiveTo).
func (t Target) LocalForwardArgs(localPort, remotePort int) []string {
	args := commonOpts(t)
	args = append(args,
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-L", strconv.Itoa(localPort)+":localhost:"+strconv.Itoa(remotePort),
		t.dest(),
	)
	return args
}

// RunInteractiveTo execs ssh wired to the current terminal and blocks until it
// exits — a shell, a Claude attach, a one-off `expose`.
//
// It is not behind the Backend seam, and unlike the terminals it does not move
// there. Those lend a terminal to a front end; this one hands over the terminal
// the caller is *sitting at*, and moving that to a client of our own means
// putting this process's stdin into raw mode, catching SIGWINCH, and restoring
// both on the way out — none of which the browser's end needs, because there is
// no terminal of ours in the middle of it.
//
// Nothing is lost by leaving it: only the CLI calls these, and the CLI exists
// only where there is a shell to type into and therefore an ssh binary to run.
// The platforms that need the library client drive the core through the UI.
//
// The session's output goes through out, so Forge can watch it for the OSC 52
// clipboard escape (see internal/clip) rather than leaving the copy to whichever
// terminal the user happens to run.
//
// stdin stays the real terminal, deliberately. ssh puts *that* fd into raw mode
// and reads the window size from it, so leaving it alone means we inherit both
// for free: no raw-mode handling of our own, no SIGWINCH plumbing, no pty in the
// middle of an interactive Claude session. Only the output is ours to read.
func RunInteractiveTo(out io.Writer, args ...string) error {
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
