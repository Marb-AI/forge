package forge

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/creack/pty"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Terminals for a front end that has no terminal of its own.
//
// The sessions in shell.go hand *this* machine's terminal to something on a
// server and block; these hand back an object instead — bytes in, bytes out, and
// a size — so a browser (and, later, a desktop or phone window) can be the far
// end. Everything about how that terminal is reached is here: the ssh argv, the
// pty, the login shell. A front end names a kind and a workspace.

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

// Terminal is one live terminal: a process on the far end of a pty. Read and
// Write carry raw bytes in both directions, escape codes and all, so the caller
// can hand them straight to a terminal emulator without interpreting anything.
//
// Close ends it, and what that means is the kind's business rather than the
// caller's: for TermClaude the far end is tmux, so closing *detaches* and the
// session (and Claude) keep running server-side; for every other kind there is no
// tmux and the shell goes with it.
type Terminal interface {
	io.Reader
	io.Writer
	// Resize tells the far end the window changed. It is what makes a resize in
	// the browser real: the pty raises SIGWINCH on the child, and ssh forwards it
	// to the remote end.
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
	cmd, err := termCmd(kind, workspace)
	if err != nil {
		return nil, err
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	t := &ptyTerm{ptmx: ptmx, cmd: cmd}
	if cols > 0 && rows > 0 {
		_ = t.Resize(cols, rows)
	}
	return t, nil
}

// termCmd is the process behind a terminal of the given kind: ssh for the three
// remote ones, your own shell for the local one.
func termCmd(kind, workspace string) (*exec.Cmd, error) {
	if kind == TermLocal {
		if workspace != "" {
			return nil, fmt.Errorf("the local terminal belongs to no workspace (got %q)", workspace)
		}
		return localShell(), nil
	}
	if workspace == "" {
		return nil, fmt.Errorf("terminal kind %q needs a workspace", kind)
	}
	h := hostFor(workspace)
	if h == nil {
		return nil, fmt.Errorf("unknown workspace %q — not created by this client", workspace)
	}
	args, err := termArgs(h, workspace, kind)
	if err != nil {
		return nil, err
	}
	return exec.Command("ssh", args...), nil
}

// termArgs is the ssh argv for a remote terminal kind. Kept apart from starting
// it so what each kind connects as — and with what forwarded — can be read, and
// tested, without a server.
func termArgs(h *config.Host, workspace, kind string) ([]string, error) {
	switch kind {
	case TermClaude:
		return sshx.WorkspaceTarget(h, workspace).TTYArgs(agentproto.AttachClaude), nil
	case TermSSH:
		// Login shell with the local SSH agent forwarded — identical to
		// `forge workspace <name> ssh`, so git in the shell uses your keys.
		return append([]string{"-A"}, sshx.WorkspaceTarget(h, workspace).TTYArgs()...), nil
	case TermHost:
		// No agent forwarding — host admin doesn't use your git keys.
		return sshx.AdminTarget(h).TTYArgs(), nil
	default:
		return nil, fmt.Errorf("unknown terminal kind %q", kind)
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

// ptyTerm is a Terminal backed by a local pty. The pty is what makes the far end
// believe it has a real terminal — and what carries a resize through ssh to the
// remote tmux client.
type ptyTerm struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func (t *ptyTerm) Read(p []byte) (int, error)  { return t.ptmx.Read(p) }
func (t *ptyTerm) Write(p []byte) (int, error) { return t.ptmx.Write(p) }

func (t *ptyTerm) Resize(cols, rows uint16) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close drops the pty and kills the process on the near end of it. For an ssh
// terminal that ends the connection, which is what makes a Claude terminal a
// detach — tmux keeps the session on the server.
func (t *ptyTerm) Close() error {
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
	return nil
}
