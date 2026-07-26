package forge

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

// What each kind connects as, and with what, is the whole difference between
// them: the same host, the same workspace, three terminals that are not
// interchangeable. A front end names the kind and gets what the name promises.
func TestEachTerminalKindConnectsAsWhatItIsFor(t *testing.T) {
	h := &config.Host{Alias: "srv", User: "admin", Addr: "203.0.113.7", Port: 2222}

	claude, err := termArgs(h, "crm", TermClaude)
	if err != nil {
		t.Fatal(err)
	}
	sshShell, err := termArgs(h, "crm", TermSSH)
	if err != nil {
		t.Fatal(err)
	}
	host, err := termArgs(h, "crm", TermHost)
	if err != nil {
		t.Fatal(err)
	}

	// Every one of them is a terminal, so every one asks ssh for a TTY: without it
	// tmux refuses to attach and a shell has no line editing.
	for name, args := range map[string][]string{"claude": claude, "ssh": sshShell, "host": host} {
		if !slices.Contains(args, "-t") {
			t.Errorf("the %s terminal does not ask for a TTY: %v", name, args)
		}
	}

	// The Claude terminal is the session, so it runs the attach — not a shell you
	// could have typed it into.
	if !slices.Contains(claude, agentproto.AttachClaude) {
		t.Errorf("the claude terminal does not attach the session: %v", claude)
	}
	if !slices.Contains(claude, "crm@203.0.113.7") {
		t.Errorf("the claude terminal does not log in as the workspace: %v", claude)
	}

	// The workspace shell forwards your agent — that is what makes git in it use
	// your keys with nothing stored on the server — and runs no remote command.
	if len(sshShell) == 0 || sshShell[0] != "-A" {
		t.Errorf("the workspace shell does not forward the SSH agent: %v", sshShell)
	}
	if last := sshShell[len(sshShell)-1]; last != "crm@203.0.113.7" {
		t.Errorf("the workspace shell runs %q instead of just logging in", last)
	}

	// The host shell is the other account: server-wide work, and deliberately
	// without your keys along for it.
	if last := host[len(host)-1]; last != "admin@203.0.113.7" {
		t.Errorf("the host terminal ends at %q, want a login as the host's own account", last)
	}
	if slices.Contains(host, "-A") {
		t.Error("the host terminal forwards your SSH agent — host admin has no business with your git keys")
	}

	// And the port the host was registered on, or ssh would go to 22.
	for name, args := range map[string][]string{"claude": claude, "ssh": sshShell, "host": host} {
		if i := slices.Index(args, "-p"); i < 0 || i+1 >= len(args) || args[i+1] != "2222" {
			t.Errorf("the %s terminal ignores the host's port: %v", name, args)
		}
	}
}

func TestAnUnknownKindIsRefused(t *testing.T) {
	if _, err := termArgs(&config.Host{}, "crm", "sftp"); err == nil {
		t.Error("an unknown terminal kind should be refused, not opened as something")
	}
}

// The local shell is the one kind that belongs to no workspace, and the three
// remote kinds are the ones that cannot exist without one. Getting either wrong
// is a caller bug, and it must be reported as one rather than opening a terminal
// onto something else.
func TestATerminalKindAndAWorkspaceMustAgree(t *testing.T) {
	if _, err := termCmd(TermLocal, "crm"); err == nil {
		t.Error("the local shell accepted a workspace; there is one local machine, not one per workspace")
	}
	for _, kind := range []string{TermClaude, TermSSH, TermHost} {
		if _, err := termCmd(kind, ""); err == nil {
			t.Errorf("the %s terminal opened without a workspace", kind)
		}
	}
	// A workspace this client does not have is not a terminal either — this
	// package's config is a throwaway one (see TestMain), so nothing is known.
	if _, err := termCmd(TermClaude, "nope"); err == nil {
		t.Error("a terminal opened onto a workspace this client has never heard of")
	}
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
