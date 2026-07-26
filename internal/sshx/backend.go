package sshx

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
)

// The transport seam: what it means to reach a server, with two implementations
// behind it.
//
// Forge has always reached its hosts by running the system's `ssh` binary, and
// that is still what happens unless you say otherwise. It cannot stay the only
// answer: a phone has no `ssh` to exec and no process to exec it with, so the
// client has to become library code eventually. This is where that swap is made
// — one interface, chosen per process, with the core's operations and the front
// ends' terminals going through it.
//
// The forwarding supervisor's tunnels are the last thing still exec'ing ssh
// directly, and they move behind this same seam in their own step: doing them
// together would mean a rewrite nobody could review against the behaviour it
// replaces. So is the CLI's own interactive session, for a different reason —
// see RunInteractiveTo.

// Backend reaches a server, in the two shapes anything Forge does to one takes.
//
// Two methods, and there is no third. Everything the operations do to a host —
// the agent, tmux, the file browser, provisioning — is "run this, feed it that,
// give me what it printed", and finishes. Everything a front end opens is a
// terminal: no output to collect, a window size, and it is held until somebody
// closes it. A seam that admits nothing else cannot grow a dialect between the
// two implementations.
type Backend interface {
	// Run executes cmd on the target and waits for it to finish. A remote command
	// that exits non-zero is reported as *ExitError, and so is a failure the
	// exec'd client can only express as an exit status — see ExitError for which
	// those are.
	Run(t Target, cmd Command) error
	// Open starts an interactive session on a terminal and hands it back live.
	Open(t Target, s Shell) (Terminal, error)
	// Name identifies the backend in errors and diagnostics.
	Name() string
}

// Shell is one interactive session: a terminal on the far end, and what to run
// on it.
//
// Remote is joined and handed to the login shell exactly as for a Command, so a
// terminal's command line does not change under a different backend either. An
// empty Remote is the login shell itself — what `ssh host` with no command does.
type Shell struct {
	Remote []string
	// Cols and Rows size the terminal before the first byte is drawn. A zero
	// dimension is filled in — see size.
	Cols, Rows uint16
	// ForwardAgent lends this machine's SSH agent to the session, which is what
	// makes git inside a workspace shell use your keys with nothing stored on the
	// server. It is `ssh -A`, and like `ssh -A` it is a request: a session whose
	// agent could not be forwarded still opens.
	ForwardAgent bool
}

func (s Shell) line() string { return joinRemote(s.Remote) }

// size is the terminal's size, with a caller who gave none answered rather than
// left to chance.
//
// "The pty's own default" is not an answer worth passing on: a freshly opened pty
// reports 0×0 on Linux, and a program handed that draws into a rectangle that
// does not exist — tmux and Claude both keep the cursor and mouse tracking of
// that first paint. Both backends fill it in here, so a terminal that opened
// unsized is the same shape whichever client opened it.
func (s Shell) size() (cols, rows uint16) {
	cols, rows = s.Cols, s.Rows
	if cols == 0 || rows == 0 {
		cols, rows = DefaultCols, DefaultRows
	}
	return cols, rows
}

// The size a terminal opens at when nobody says otherwise — the conventional one,
// which is also what a terminal emulator opens a window at. Exported because the
// local shell is sized the same way and does not come through this seam (see
// forge.OpenTerminal).
const (
	DefaultCols = 80
	DefaultRows = 24
)

// Terminal is one live terminal: something running on the far end of a pty, with
// no output to collect and no exit status to read. Read and Write carry raw
// bytes both ways, escape codes and all, so whatever holds it can hand them
// straight to a terminal emulator.
//
// Where the pty is is the difference between the backends and the whole point of
// this step: the exec'd client puts one on *this* machine in front of ssh, while
// Forge's own client asks the server for one over the connection — which is the
// only version of it a phone can have.
type Terminal interface {
	io.Reader
	io.Writer
	// Resize tells the far end the window changed, so a resize in a browser is a
	// SIGWINCH on the program drawing into it.
	Resize(cols, rows uint16) error
	io.Closer
}

// Command is one non-interactive remote command.
//
// Remote is the argv as the caller wrote it, and both backends join it with
// single spaces and hand the result to the host's login shell — which is
// exactly what `ssh host a b c` does. Quoting is therefore the caller's
// business, as it always was, and does not change under a different backend.
//
// A nil stream means the same as /dev/null: no input, output discarded.
type Command struct {
	Remote []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// line is the command string sent to the remote shell — the one thing both
// backends must agree on to the byte.
func (c Command) line() string { return joinRemote(c.Remote) }

// joinRemote is that agreement, shared by commands and terminals alike: single
// spaces, and the host's login shell parses the result. It is what `ssh host a b
// c` does with its trailing arguments, so quoting stays the caller's business
// and means the same thing whichever client runs it.
func joinRemote(remote []string) string { return strings.Join(remote, " ") }

// ExitError reports that the remote command ran and exited non-zero. Code is
// its exit status.
//
// Both backends report it as this type, so an operation can read the status
// without knowing what carried it: the exec'd ssh learns it from the process it
// waited on, the pure-Go client from the exit-status message on the channel.
//
// One difference is worth naming, because no wrapper can hide it: with the
// exec'd backend a *connection* failure also arrives as an exit status (ssh's
// own 255), while the Go client returns a dial or handshake error instead.
// Nothing reads 255 as anything but a failure, which is why this is a note
// rather than a translation layer.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode makes the status readable without naming this type — see fsErr in
// the core, which reads the exit codes a remote snippet uses to say why it
// failed.
func (e *ExitError) ExitCode() int { return e.Code }

// backendEnv selects the transport for the process.
//
// An environment variable rather than a setting, because this is not a
// preference: it is which client we are testing today. The pure-Go one is
// built, exercised against a real server, and left switched off — flipping the
// default is its own change, with the device key and a known-hosts store of
// Forge's own behind it, and until then a Forge that silently stopped using
// your ~/.ssh config would be a mystery, not a feature.
const backendEnv = "FORGE_SSH_BACKEND"

// chosen is the backend a front end wired in explicitly, if any. Nil means "ask
// the environment", which is what every Forge in existence does today.
//
// Process-wide and set at startup, like the core's stores: one process talks to
// its servers one way, and changing that under a running daemon would leave
// connections it opened answering to a client nobody selected.
var (
	chosenMu sync.Mutex
	chosen   Backend
)

// Use points the transport at a backend of the caller's choosing. It is how the
// default gets flipped later, and how a test substitutes a backend that never
// touches the network.
func Use(b Backend) {
	chosenMu.Lock()
	defer chosenMu.Unlock()
	chosen = b
}

// backend returns the transport to run the next command on.
func backend() Backend {
	chosenMu.Lock()
	defer chosenMu.Unlock()
	if chosen != nil {
		return chosen
	}
	if os.Getenv(backendEnv) == "go" {
		return goBackend{}
	}
	return execBackend{}
}

// Output runs a remote command and returns its stdout.
//
// Stderr is left attached to this process's, deliberately: an authentication or
// host-key problem is printed there and nowhere else, and a caller that only
// wanted the output would otherwise swallow the one line explaining why there
// is none.
func (t Target) Output(remote ...string) ([]byte, error) {
	var out bytes.Buffer
	err := backend().Run(t, Command{
		Remote: remote,
		Stdout: &out,
		Stderr: os.Stderr,
	})
	return out.Bytes(), err
}

// Pipe runs a remote command with stdin taken from r and its two output streams
// going where the caller says.
//
// Both writers are named rather than merged because the callers differ on it:
// piping a provisioning script sends both to one stream, so a follower reads
// the errors in order with the progress, while a background command keeps them
// apart on this process's own stdout and stderr.
func (t Target) Pipe(r io.Reader, stdout, stderr io.Writer, remote ...string) error {
	return backend().Run(t, Command{
		Remote: remote,
		Stdin:  r,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// Open starts an interactive session on the target and returns the terminal it
// is attached to. The caller owns it and must Close it.
func (t Target) Open(s Shell) (Terminal, error) {
	return backend().Open(t, s)
}
