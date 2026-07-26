// Package localpty starts a process on this machine behind a pseudo-terminal and
// hands back the terminal rather than the process.
//
// It is the only place in Forge that knows what a pty is, and it has exactly two
// callers. One is the local login shell — the terminal in the UI rail that never
// touches ssh. The other is the exec'd SSH backend: the ssh binary wants a
// terminal on its own stdin, and the pty is both what convinces it of that and
// what carries a window change through to the far end as SIGWINCH.
//
// Forge's own SSH client needs none of this. Its terminal is a pty on the
// *server*, asked for over the connection (see sshx), so on the platforms where
// there is no process to start there is also nothing here to start it with.
package localpty

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// Term is a process on the near end of a pty: raw bytes in, raw bytes out, and a
// window size. It is deliberately not an interface — the packages that hand one
// on describe it in their own terms (forge.Terminal, sshx.Terminal), and this is
// the thing that satisfies them.
type Term struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// Start runs cmd with a pty in front of it, sized to cols×rows.
//
// The size is given at start rather than set afterwards because the first thing
// the child does is draw: a full-screen program resized a moment after it opened
// has already painted into the wrong rectangle, and tmux in particular keeps the
// cursor and mouse tracking of that first one.
//
// A zero dimension leaves the pty at whatever it defaults to, which is 0×0 on
// Linux and therefore no use to anything drawing into it. Forge's callers do not
// pass one — they fill it in with a conventional size first (sshx.DefaultCols,
// forge.OpenTerminal) — and this stays as the honest answer for a caller that
// really has no size to give.
func Start(cmd *exec.Cmd, cols, rows uint16) (*Term, error) {
	var size *pty.Winsize
	if cols > 0 && rows > 0 {
		size = &pty.Winsize{Cols: cols, Rows: rows}
	}
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, err
	}
	return &Term{ptmx: ptmx, cmd: cmd}, nil
}

func (t *Term) Read(p []byte) (int, error)  { return t.ptmx.Read(p) }
func (t *Term) Write(p []byte) (int, error) { return t.ptmx.Write(p) }

// Resize tells the child the window changed: the pty raises SIGWINCH on it, and
// ssh forwards the new size to the remote end.
func (t *Term) Resize(cols, rows uint16) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close drops the pty and kills the process on the near end of it.
//
// For an ssh terminal that ends the connection, which is what makes a Claude
// terminal a detach rather than an exit: tmux keeps the session on the server.
func (t *Term) Close() error {
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
	return nil
}
