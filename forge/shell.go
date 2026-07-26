package forge

import (
	"errors"
	"io"
	"os/exec"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// The sessions that hand this machine's terminal to something running on a
// server and block until it exits: a shell in the workspace, the Claude session,
// a one-off tunnel held open by hand.
//
// They take an io.Writer for the same reason Checkpoint does — the output is
// somebody's to show. The CLI passes a writer that watches for the clipboard
// escape on its way to the screen; a front end with no terminal of its own has
// no business calling these at all.

// ExitError reports that a session ran and ended with a non-zero status: Ctrl-C
// out of a shell, a remote command that failed, a connection dropped mid-session.
//
// It is not a Forge failure and there is nothing to add to it — whatever went
// wrong has already been said on the terminal the session was attached to. A
// caller distinguishes it to decide what NOT to print: an unknown workspace
// deserves an explanation, a Ctrl-C deserves silence.
type ExitError struct{ Err error }

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// Shell opens a login shell as the workspace user. With agent set, the local SSH
// agent is forwarded, so git operations inside the workspace use your keys with
// no credential ever stored on the server.
func Shell(name string, agent bool, out io.Writer) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	args := target.TTYArgs()
	if agent {
		args = append([]string{"-A"}, args...)
	}
	return interactive(out, args)
}

// AttachClaude attaches to the workspace's Claude session, starting one if there
// is none. With renew set the existing session is killed first, which is how you
// ask for a fresh context.
//
// tmux is what makes the session persistent: detaching (Ctrl-b d) leaves it to
// reattach later, while /exit or Ctrl-C ends Claude, the command finishes and the
// tmux session is gone — so the next attach is a clean new session. A killed
// session stays killed and is never offered for resume.
//
// Remote Control is deliberately not auto-started here: its resume-the-last-
// session behaviour breaks that guarantee. Run /remote-control inside the session
// to surface it in the Claude app; it is named after the workspace already, via
// CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX in the environment.
func AttachClaude(name string, renew bool, out io.Writer) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	// Attach-or-create in one command, so a disconnect costs a reattach and
	// nothing else. Renewing kills first, in the same round trip.
	remote := agentproto.AttachClaude
	if renew {
		remote = agentproto.KillClaude + "; " + remote
	}
	return interactive(out, target.TTYArgs(remote))
}

// KillClaude kills the workspace's Claude tmux session, failing when there was
// none to kill.
//
// Not StopSession, which also clears the session's clocks and succeeds against a
// session that is already gone. Two operations with one word for them is not
// ideal, but reconciling them means deciding which of the two a stop should be,
// and that decision predates this package: the CLI's `claude stop` has always
// been the terse one, the UI's stop button the thorough one. They sit next to
// each other now, which is at least the honest version of the difference.
func KillClaude(name string) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	return runCapture(target, "tmux", "kill-session", "-t", agentproto.TmuxSession)
}

// ExposePort holds a single tunnel from this machine's port to the same port in
// the workspace, in the foreground, until the caller interrupts it. The
// always-on equivalent is the forwarding supervisor, which needs no one watching.
func ExposePort(name string, port int, out io.Writer) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	return interactive(out, target.LocalForwardArgs(port, port))
}

// interactive runs ssh attached to this terminal and blocks until it exits,
// marking a non-zero exit as an ExitError. Anything else — no ssh binary, a
// process that could not be started — is a real failure and is returned as one.
func interactive(out io.Writer, args []string) error {
	err := sshx.RunInteractiveTo(out, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitError{Err: err}
	}
	return err
}
