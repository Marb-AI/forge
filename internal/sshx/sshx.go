// Package sshx builds and runs ssh commands. It is the single place that knows
// how Forge shells out to the system ssh client — so keys, ~/.ssh/config and
// known_hosts all "just work" and we never reimplement crypto.
package sshx

import (
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/Marb-AI/forge/internal/config"
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
		// A *changed* key still fails loudly — that's a real warning.
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
func (t Target) Args(remote ...string) []string {
	args := commonOpts(t.Port)
	args = append(args, t.dest())
	args = append(args, remote...)
	return args
}

// TTYArgs returns the ssh argv for an interactive command (-t forces a TTY),
// used for shells and tmux attach.
func (t Target) TTYArgs(remote ...string) []string {
	args := append([]string{"-t"}, commonOpts(t.Port)...)
	args = append(args, t.dest())
	args = append(args, remote...)
	return args
}

// LocalForwardArgs returns the ssh argv for a single local port forward with no
// remote command (-N). This is what the supervisor runs per port.
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

// RunInteractive execs ssh wired to the current terminal and blocks until it
// exits. Used for shells, Claude attach and one-off `expose`.
func RunInteractive(args ...string) error {
	return RunInteractiveTo(os.Stdout, args...)
}

// RunInteractiveTo is RunInteractive with the session's output going through out
// — so Forge can watch it for the OSC 52 clipboard escape (see internal/clip)
// rather than leaving the copy to whichever terminal the user happens to run.
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

// Capture runs ssh and returns stdout. Stderr is left attached so auth/host-key
// problems are visible to the user.
func Capture(args ...string) ([]byte, error) {
	cmd := exec.Command("ssh", args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// RunWithInput runs ssh with stdin taken from r and stdout/stderr streamed to
// the terminal — each to its own stream, so redirecting one doesn't capture the
// other. Used to pipe a provisioning script (or a binary) to the host.
func RunWithInput(r io.Reader, args ...string) error {
	return runWithInput(r, os.Stdout, os.Stderr, args...)
}

// RunWithInputTo is RunWithInput with the output going wherever the caller wants
// — an SSE stream for the browser UI — so a long provisioning run can be watched
// from either front end. stdout and stderr are merged, because a follower reads
// one stream and wants the errors in it, in order.
func RunWithInputTo(r io.Reader, out io.Writer, args ...string) error {
	return runWithInput(r, out, out, args...)
}

func runWithInput(r io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = r
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
