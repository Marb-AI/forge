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
	"io"
	"os"
	"os/exec"
	"strconv"

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
func commonOpts(port int) []string {
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
		// TOFU: record a new server's host key on first connect instead of
		// refusing non-interactively (you own the servers Forge connects to).
		// A *changed* key still fails loudly — that's a real warning. The
		// pure-Go client follows the same policy in a file of Forge's own,
		// because this option writes to one that is OpenSSH's — knownhosts.go.
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if port != 0 && port != 22 {
		opts = append(opts, "-p", strconv.Itoa(port))
	}
	return opts
}

// Target is a resolved SSH destination.
type Target struct {
	User string // login user (admin for agent ops, or the workspace name)
	Addr string
	Port int
}

func (t Target) dest() string { return t.User + "@" + t.Addr }

// AdminTarget is the host's admin account (used to invoke forge-agent).
func AdminTarget(h *config.Host) Target {
	return Target{User: h.User, Addr: h.Addr, Port: h.Port}
}

// WorkspaceTarget reaches a workspace as its own Linux user at the host address.
func WorkspaceTarget(h *config.Host, workspace string) Target {
	return Target{User: workspace, Addr: h.Addr, Port: h.Port}
}

// Args returns the ssh argv for a non-interactive remote command (no TTY).
//
// The exec'd backend's own business — an operation asks for Output or Pipe and
// never sees an argv, because there is no argv when the pure-Go client runs it.
func (t Target) Args(remote ...string) []string {
	args := commonOpts(t.Port)
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
	args := append([]string{"-t"}, commonOpts(t.Port)...)
	args = append(args, t.dest())
	args = append(args, remote...)
	return args
}

// ttyArgs is the argv for a Shell — the same interactive argv, plus the one
// option a front end's terminal can ask for by itself.
//
// -A goes first, before the options and the destination, because that is where
// the workspace shell has always put it: the argv this produces is byte-for-byte
// the one Forge ran when the UI built its own.
func (t Target) ttyArgs(s Shell) []string {
	var args []string
	if s.ForwardAgent {
		args = append(args, "-A")
	}
	return append(args, t.TTYArgs(s.Remote...)...)
}

// LocalForwardArgs returns the ssh argv for a single local port forward with no
// remote command (-N).
//
// The supervisor's tunnels no longer come through here — they ask the target to
// Forward a port, and only the exec'd backend turns that into an argv. What is
// left is `forge expose`, the one tunnel held in the CLI's own foreground, for
// the same reason its interactive sessions are (see RunInteractiveTo).
func (t Target) LocalForwardArgs(localPort, remotePort int) []string {
	args := commonOpts(t.Port)
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
