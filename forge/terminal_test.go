package forge

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// What each kind connects as, and with what, is the whole difference between
// them: the same host, the same workspace, three terminals that are not
// interchangeable. A front end names the kind and gets what the name promises.
//
// It is now said in the transport's own terms — who to log in as, what to run,
// whether the agent goes along — rather than as an ssh argv, because a terminal
// on a phone has no argv to be described by. What the exec'd client makes of it
// is its business, and its own test.
func TestEachTerminalKindConnectsAsWhatItIsFor(t *testing.T) {
	h := &config.Host{Alias: "srv", User: "admin", Addr: "203.0.113.7", Port: 2222}
	workspace := sshx.Target{User: "crm", Addr: "203.0.113.7", Port: 2222}
	admin := sshx.Target{User: "admin", Addr: "203.0.113.7", Port: 2222}

	claudeTarget, claude, err := termShell(h, "crm", TermClaude)
	if err != nil {
		t.Fatal(err)
	}
	sshTarget, sshShell, err := termShell(h, "crm", TermSSH)
	if err != nil {
		t.Fatal(err)
	}
	hostTarget, host, err := termShell(h, "crm", TermHost)
	if err != nil {
		t.Fatal(err)
	}

	// The Claude terminal is the session, so it runs the attach — not a shell you
	// could have typed it into — as the workspace's own user.
	if claudeTarget != workspace {
		t.Errorf("the claude terminal logs in as %+v, want the workspace %+v", claudeTarget, workspace)
	}
	if !slices.Contains(claude.Remote, agentproto.AttachClaude) {
		t.Errorf("the claude terminal does not attach the session: %v", claude.Remote)
	}
	// The workspace shell is a login shell as the workspace, with nothing lent to
	// it: git here uses the identity `host prepare` put on the server, the same
	// one Claude's session uses. It used to carry your forwarded agent, which
	// meant the same repository was pushed under two identities depending on who
	// ran the command.
	if sshTarget != workspace {
		t.Errorf("the workspace shell logs in as %+v, want the workspace %+v", sshTarget, workspace)
	}
	if len(sshShell.Remote) != 0 {
		t.Errorf("the workspace shell runs %v instead of just logging in", sshShell.Remote)
	}

	// The host shell is the other account: server-wide work, and deliberately
	// without your keys along for it.
	if hostTarget != admin {
		t.Errorf("the host terminal logs in as %+v, want the host's own account %+v", hostTarget, admin)
	}
	if len(host.Remote) != 0 {
		t.Errorf("the host terminal runs %v instead of just logging in", host.Remote)
	}
}

func TestAnUnknownKindIsRefused(t *testing.T) {
	if _, _, err := termShell(&config.Host{}, "crm", "sftp"); err == nil {
		t.Error("an unknown terminal kind should be refused, not opened as something")
	}
}

// The local shell is the one kind that belongs to no workspace, and the three
// remote kinds are the ones that cannot exist without one. Getting either wrong
// is a caller bug, and it must be reported as one rather than opening a terminal
// onto something else.
func TestATerminalKindAndAWorkspaceMustAgree(t *testing.T) {
	if _, err := OpenTerminal(TermLocal, "crm", 80, 24); err == nil {
		t.Error("the local shell accepted a workspace; there is one local machine, not one per workspace")
	}
	for _, kind := range []string{TermClaude, TermSSH, TermHost} {
		if _, err := OpenTerminal(kind, "", 80, 24); err == nil {
			t.Errorf("the %s terminal opened without a workspace", kind)
		}
	}
	// A workspace this client does not have is not a terminal either — this
	// package's config is a throwaway one (see TestMain), so nothing is known.
	if _, err := OpenTerminal(TermClaude, "nope", 80, 24); err == nil {
		t.Error("a terminal opened onto a workspace this client has never heard of")
	}
}

// The size comes from the window rather than from the kind, and it has to reach
// the transport: that is the only thing that can size a terminal before its first
// draw, and a Claude session that opens at the wrong size stays wrong until
// something redraws it.
func TestOpeningARemoteTerminalHandsItToTheTransportWithTheWindowSize(t *testing.T) {
	swapState(t, t.TempDir())
	store, err := Store()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Hosts["srv"] = &config.Host{Alias: "srv", User: "admin", Addr: "203.0.113.7", Port: 2222}
		c.Workspaces["crm"] = "srv"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	transport := useFakeTransport(t)

	term, err := OpenTerminal(TermSSH, "crm", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if transport.target.User != "crm" || transport.target.Port != 2222 {
		t.Errorf("the terminal was opened on %+v, want the workspace on its host's port", transport.target)
	}
	if transport.shell.Cols != 100 || transport.shell.Rows != 30 {
		t.Errorf("the transport was given %dx%d, want the 100x30 the window asked for",
			transport.shell.Cols, transport.shell.Rows)
	}
}

// fakeTransport is a backend that opens nothing: it records what it was asked
// for, so the wiring from a kind to a session can be read without a server.
type fakeTransport struct {
	target sshx.Target
	shell  sshx.Shell
}

func (*fakeTransport) Name() string                        { return "fake" }
func (*fakeTransport) Run(sshx.Target, sshx.Command) error { return nil }

// Nothing in this package forwards a port — that is the supervisor's, and it is
// the one shape of the transport these tests never ask for.
func (*fakeTransport) Forward(sshx.Target, int, int) (sshx.Tunnel, error) {
	return nil, errors.New("this backend carries no ports")
}

func (f *fakeTransport) Open(t sshx.Target, s sshx.Shell) (sshx.Terminal, error) {
	f.target, f.shell = t, s
	return deadTerm{}, nil
}

type deadTerm struct{}

func (deadTerm) Read([]byte) (int, error)    { return 0, io.EOF }
func (deadTerm) Write(p []byte) (int, error) { return len(p), nil }
func (deadTerm) Resize(uint16, uint16) error { return nil }
func (deadTerm) Close() error                { return nil }

// useFakeTransport points the transport at a backend that reaches no server, and
// puts the process back to the default afterwards — which, in this package's
// tests, is the only thing it has ever been.
func useFakeTransport(t *testing.T) *fakeTransport {
	t.Helper()
	f := &fakeTransport{}
	sshx.Use(f)
	t.Cleanup(func() { sshx.Use(nil) })
	return f
}

// The local shell is the one terminal Forge opens without ssh, so the things that
// make it usable are the ones nothing else checks: it must be a LOGIN shell (your
// profile, so your PATH and aliases), it must start in your home directory rather
// than wherever the daemon happened to be launched from, and it must tell the far
// end it is talking to a real terminal — a window with a terminal emulator in it —
// or vim and htop draw into a teletype.
func TestTheLocalShellIsALoginShellInYourHome(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	// A daemon inheriting a TERM of its own is the case worth pinning: it must be
	// replaced, not joined by a second entry.
	t.Setenv("TERM", "dumb")

	cmd := localShell()

	// argv[0] with a leading dash is the only portable way to say "login shell";
	// the -l flag is not one /bin/sh understands.
	if got := cmd.Args[0]; got != "-sh" {
		t.Errorf("argv[0] = %q, want %q — the shell would not read your profile", got, "-sh")
	}
	if home, err := os.UserHomeDir(); err == nil && cmd.Dir != home {
		t.Errorf("shell starts in %q, want your home %q", cmd.Dir, home)
	}
	// Exactly one TERM, and the window's: two entries would leave the answer to
	// whoever reads the environment, and the daemon's own TERM is the one thing it
	// must not pass on — it describes the terminal the daemon was started from, not
	// the one on the other end.
	if got := envValues(cmd.Env, "TERM"); len(got) != 1 || got[0] != "xterm-256color" {
		t.Errorf("TERM entries = %q, want exactly one xterm-256color — "+
			"full-screen programs would render for the wrong terminal", got)
	}
}

// And it has to actually BE a shell: open one, run a command through it, and read
// the output back — the round trip a front end makes, with no ssh in it.
func TestTheLocalShellRunsWhatYouTypeIntoIt(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")

	term, err := OpenTerminal(TermLocal, "", 80, 24)
	if err != nil {
		t.Fatalf("open the local shell: %v", err)
	}
	defer term.Close()

	// The command is written as fo""rge-ok so the line the shell echoes back is not
	// itself the marker — only real output of a real shell can produce it.
	if _, err := term.Write([]byte("echo fo\"\"rge-ok\n")); err != nil {
		t.Fatalf("write to the shell: %v", err)
	}
	if out, ok := readUntil(term, "forge-ok", 20*time.Second); !ok {
		t.Errorf("the shell never ran the command; read so far:\n%s", out)
	}
}

// The local shell does not go through the transport, so the size a caller left out
// has to be filled in here — or it is the one terminal in the rail that opens into
// a pty its own kernel reports as 0×0.
func TestTheLocalShellOpensAtTheConventionalSizeWhenNoneWasGiven(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")

	term, err := OpenTerminal(TermLocal, "", 0, 0)
	if err != nil {
		t.Fatalf("open the local shell: %v", err)
	}
	defer term.Close()

	// stty reads the size off the terminal it is attached to, so this is the shell's
	// own view of the window rather than what we asked for.
	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	if out, ok := readUntil(term, "24 80", 20*time.Second); !ok {
		t.Errorf("the shell does not see an 80x24 window; read so far:\n%s", out)
	}
}

// envValues returns every value a process environment gives key — every one,
// because "how many" is half of what the caller is checking.
func envValues(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

// readUntil reads the terminal until marker shows up or the deadline passes,
// returning everything it read (for the failure message) and whether it found it.
func readUntil(term Terminal, marker string, within time.Duration) (string, bool) {
	out := make(chan []byte, 32)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				out <- b
			}
			if err != nil {
				close(out)
				return
			}
		}
	}()

	deadline := time.After(within)
	var seen bytes.Buffer
	for {
		select {
		case b, ok := <-out:
			if !ok {
				return seen.String(), false // the shell exited without saying it
			}
			seen.Write(b)
			if strings.Contains(seen.String(), marker) {
				return seen.String(), true
			}
		case <-deadline:
			return seen.String(), false
		}
	}
}
