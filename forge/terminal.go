package forge

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/localpty"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Terminals for a front end that has no terminal of its own.
//
// The sessions in shell.go hand *this* machine's terminal to something on a
// server and block; these hand back an object instead — bytes in, bytes out, and
// a size — so a browser (and, later, a desktop or phone window) can be the far
// end. What is here is which account each kind reaches and what it runs there;
// how a terminal is obtained is the transport's business (sshx), and the answer
// differs per backend — a pty in front of ssh on this machine, or one asked of
// the server. The exception is the local shell, which has no server in it.

// The kinds of terminal Forge opens.
const (
	// TermClaude attaches the workspace's persistent Claude session (tmux):
	// closing the terminal detaches, the session lives on.
	TermClaude = "claude"
	// TermSSH is a plain login shell as the workspace user — the panel you pop
	// open to run one command. It is NOT tmux-backed, so it lives exactly as long
	// as the terminal holding it.
	TermSSH = "ssh"
	// TermHost is a login shell as the host's own login account — the user
	// `host prepare` connected as (root, or a passwordless-sudo user; it differs
	// per server). Unlike TermSSH it is not scoped to a workspace user: it is the
	// shell for server-wide work like installing a package. Like TermSSH it is not
	// tmux-backed and lives with its terminal.
	TermHost = "host"
	// TermLocal is a login shell on THIS machine — the one running Forge. It
	// belongs to no workspace, so it is the one kind opened with an empty name.
	TermLocal = "local"
)

// Terminal is one live terminal: something running on the far end of a pty. Read
// and Write carry raw bytes in both directions, escape codes and all, so the
// caller can hand them straight to a terminal emulator without interpreting
// anything.
//
// Close ends it, and what that means is the kind's business rather than the
// caller's: for TermClaude the far end is tmux, so closing *detaches* and the
// session (and Claude) keep running server-side; for every other kind there is no
// tmux and the shell goes with it.
type Terminal interface {
	io.Reader
	io.Writer
	// Resize tells the far end the window changed. It is what makes a resize in
	// the browser real: the program drawing into the terminal gets a SIGWINCH.
	Resize(cols, rows uint16) error
	io.Closer
}

// OpenTerminal opens a terminal of the given kind, sized to cols×rows so the
// very first draw matches the window it is going to (a 0×0 or default pty makes
// tmux and Claude render into the wrong rectangle — cursor adrift, mouse tracking
// off). A zero dimension leaves the pty at its default.
//
// workspace names the workspace for the three remote kinds and must be empty for
// TermLocal, which belongs to no workspace: there is one local machine, so there
// is one local shell.
func OpenTerminal(kind, workspace string, cols, rows uint16) (Terminal, error) {
	if kind == TermLocal {
		if workspace != "" {
			return nil, fmt.Errorf("the local terminal belongs to no workspace (got %q)", workspace)
		}
		// Unwrapped rather than returned as a pair: a *localpty.Term that is nil
		// alongside an error would still be a non-nil Terminal to whoever ignored
		// the error.
		term, err := localpty.Start(localShell(), cols, rows)
		if err != nil {
			return nil, err
		}
		return term, nil
	}
	if workspace == "" {
		return nil, fmt.Errorf("terminal kind %q needs a workspace", kind)
	}
	h := hostFor(workspace)
	if h == nil {
		return nil, fmt.Errorf("unknown workspace %q — not created by this client", workspace)
	}
	target, shell, err := termShell(h, workspace, kind)
	if err != nil {
		return nil, err
	}
	shell.Cols, shell.Rows = cols, rows
	return target.Open(shell)
}

// termShell is who a remote terminal kind logs in as and what it runs there.
// Kept apart from opening it so the difference between the kinds — the whole
// reason there are three — can be read, and tested, without a server.
func termShell(h *config.Host, workspace, kind string) (sshx.Target, sshx.Shell, error) {
	switch kind {
	case TermClaude:
		return sshx.WorkspaceTarget(h, workspace), sshx.Shell{Remote: []string{agentproto.AttachClaude}}, nil
	case TermSSH:
		// Login shell with the local SSH agent forwarded — identical to
		// `forge workspace <name> ssh`, so git in the shell uses your keys.
		return sshx.WorkspaceTarget(h, workspace), sshx.Shell{ForwardAgent: true}, nil
	case TermHost:
		// No agent forwarding — host admin doesn't use your git keys.
		return sshx.AdminTarget(h), sshx.Shell{}, nil
	default:
		return sshx.Target{}, sshx.Shell{}, fmt.Errorf("unknown terminal kind %q", kind)
	}
}

// localShell builds the login shell for TermLocal, starting in your home
// directory.
//
// It is the one terminal Forge opens that never touches ssh: the other shells are
// there to save you a terminal window for the servers, and this one is there so
// the local commands that go with them (a git push from a clone here, a scp, a
// curl at the tunnel) don't send you out of the tool either.
func localShell() *exec.Cmd {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	cmd := exec.Command(sh)
	// argv[0] with a leading dash is how a unix shell is told it is a login shell
	// — the same thing your terminal app does when it opens a window — so it reads
	// your profile and you get the PATH, aliases and prompt you actually have. The
	// `-l` flag would do it for bash/zsh but not for a plain sh, and $SHELL is
	// whatever the user chose; the dash convention holds for all of them.
	cmd.Args = []string{"-" + filepath.Base(sh)}
	// Home, not the process's cwd: the UI daemon is started from wherever you
	// happened to be standing and then detaches, so its directory is an accident.
	// Falling back to inheriting it is still better than failing to open a shell.
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	// A daemon has no terminal of its own, so TERM is whatever it inherited —
	// often unset, sometimes the terminal it was started from. The far end is a
	// terminal emulator in a window: say so, or full-screen programs (vim, htop,
	// less) draw as if into a teletype. Replaced rather than appended, so the shell
	// is handed exactly one TERM: os/exec would keep the last of two, but which of
	// a duplicate pair wins is not a rule this should ask anyone to remember.
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "TERM=")
	})
	cmd.Env = append(env, "TERM=xterm-256color")
	return cmd
}
