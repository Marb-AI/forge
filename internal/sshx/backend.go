package sshx

import (
	"bytes"
	"errors"
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
// What is left outside it is the CLI's own interactive session, for a reason of
// its own — see RunInteractiveTo.

// Backend reaches a server, in the three shapes anything Forge does to one takes.
//
// Something runs and finishes: everything the operations do to a host — the
// agent, tmux, the file browser, provisioning — is "run this, feed it that, give
// me what it printed". Something is held open with a terminal on it: what a front
// end opens has no output to collect, has a window size, and lives until somebody
// closes it. And something is held open with a local port on it: a tunnel carries
// whatever connects to it, for as long as it is left alone.
//
// Three, and the third arrived a step later than the first two — the supervisor's
// tunnels were the last thing still exec'ing ssh on their own. It is a genuinely
// separate shape rather than one of the others worn loosely: the backends have
// nothing in common across them (a process, a pty, a listener), and a method wide
// enough to cover all three is exactly where a dialect between two clients starts.
type Backend interface {
	// Run executes cmd on the target and waits for it to finish. A remote command
	// that exits non-zero is reported as *ExitError, and so is a failure the
	// exec'd client can only express as an exit status — see ExitError for which
	// those are.
	Run(t Target, cmd Command) error
	// Open starts an interactive session on a terminal and hands it back live.
	Open(t Target, s Shell) (Terminal, error)
	// Forward carries local, on this machine's loopback, to remote on the far end.
	Forward(t Target, local, remote int) (Tunnel, error)
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

// Tunnel is one live local port forward: a port on this machine's loopback whose
// connections come out on the far end of the link.
//
// It has no streams of its own, because nothing here reads it — whatever connects
// to the local port is the traffic. What the holder needs is the other two things:
// when it stopped, and how to stop it.
//
// Lazy, as `ssh -L` is: a tunnel to a service that is not listening is a tunnel,
// not a failure. Nothing dials the far end until something dials the near one, so
// a container that is down costs the tunnel nothing and it is there when the
// container comes back.
type Tunnel interface {
	// Wait blocks until the tunnel stops carrying anything and reports why. It
	// returns nil when the tunnel was closed by whoever held it, and an error —
	// possibly ErrAuth or ErrPortBusy — when it stopped by itself.
	Wait() error
	// Close takes the tunnel down and releases the local port. It returns only
	// once that port is free, so the holder can rebind it immediately, and it is
	// safe to call more than once and alongside Wait.
	io.Closer
}

// The two failures a tunnel has that a caller acts on differently, named here so
// that acting on them does not mean reading a particular client's diagnostics.
//
// Everything else is "it stopped, try again in a second": a refused connection, a
// server rebooting, a link that dropped. These two are not that. An authentication
// failure never fixes itself, so retrying it is a loop that runs forever and
// achieves nothing; a local port held by another program is not the server's fault
// at all, and clears the moment that program goes.
var (
	// ErrAuth is the server refusing this client's credentials.
	ErrAuth = errors.New("authentication failed")
	// ErrPortBusy is the local port already being held on this machine.
	ErrPortBusy = errors.New("the local port is already in use")
)

// Forward carries this machine's local port to the same port's worth of the far
// end, and hands back the tunnel to hold. The caller owns it and must Close it.
func (t Target) Forward(local, remote int) (Tunnel, error) {
	return backend().Forward(t, local, remote)
}

// authFailed and portBusy read a failure that arrived as prose.
//
// Neither client offers anything better: OpenSSH says why it gave up on its
// stderr and exits 255 like it does for everything else, and x/crypto's handshake
// error is a formatted string. So the transport reads the prose — once, here, in
// the place that knows which clients produce it — and the supervisor above it
// switches on ErrAuth and ErrPortBusy instead of on either client's wording.
func authFailed(text string) bool {
	s := strings.ToLower(text)
	return strings.Contains(s, "permission denied") ||
		strings.Contains(s, "publickey") ||
		strings.Contains(s, "too many authentication failures") ||
		// x/crypto's own phrasing, which shares none of OpenSSH's words.
		strings.Contains(s, "unable to authenticate")
}

// portBusy reports whether a failure is the LOCAL port being taken. OpenSSH is
// told to give up immediately when a forward cannot be set up
// (ExitOnForwardFailure=yes) and says which of these it was.
func portBusy(text string) bool {
	s := strings.ToLower(text)
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "cannot listen to port")
}

// firstLine is the first line of a diagnostic, which is the part worth repeating:
// ssh follows a failure with advice about known_hosts and key permissions that
// says nothing about this tunnel.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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

// backendEnv is the way back to the ssh binary.
//
// Forge's own client is the default now. It has the device key behind it, which
// was the condition: until Forge had a key of its own, switching would have
// meant silently ignoring your ~/.ssh config, and a Forge that stopped using
// your keys without being asked is a mystery rather than a feature. With a key
// it created, installed on the servers it prepared and in the workspaces it
// made, there is nothing left to borrow and no surprise to spring.
//
// The variable stays, pointing the other way: FORGE_SSH_BACKEND=exec puts the
// system ssh back for one process. It is not a preference to be set and
// forgotten — it is what you reach for when the new client is suspected, and
// what makes "is it this, or is it me" answerable in one command.
const backendEnv = "FORGE_SSH_BACKEND"

// chosen is the backend a front end wired in explicitly, if any. Nil means "ask
// the environment", which is what every Forge does unless a test says otherwise.
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
	if os.Getenv(backendEnv) == "exec" {
		return execBackend{}
	}
	return goBackend{}
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
