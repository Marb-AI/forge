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

// commonOpts are applied to every connection: fail fast on a dead server rather
// than hanging on a long TCP timeout, and never prompt interactively for a
// password (Forge is key-only).
func commonOpts(port int) []string {
	opts := []string{
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "BatchMode=no", // allow key passphrase prompts, but not password auth
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
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
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
// the terminal. Used to pipe a provisioning script (or a binary) to the host.
func RunWithInput(r io.Reader, args ...string) error {
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
