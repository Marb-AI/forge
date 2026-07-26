package sshx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeBackend records what it was asked to run and answers with what the test
// wants back, so the plumbing above the seam can be checked without a server.
type fakeBackend struct {
	target Target
	cmd    Command
	shell  Shell
	stdin  string
	stdout string
	stderr string
	err    error
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Open(t Target, s Shell) (Terminal, error) {
	f.target, f.shell = t, s
	if f.err != nil {
		return nil, f.err
	}
	return fakeTerm{}, nil
}

// fakeTerm is a terminal that does nothing, for the callers that only care that
// one was opened.
type fakeTerm struct{}

func (fakeTerm) Read([]byte) (int, error)    { return 0, io.EOF }
func (fakeTerm) Write(p []byte) (int, error) { return len(p), nil }
func (fakeTerm) Resize(uint16, uint16) error { return nil }
func (fakeTerm) Close() error                { return nil }

func (f *fakeBackend) Run(t Target, c Command) error {
	f.target, f.cmd = t, c
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		f.stdin = string(b)
	}
	if c.Stdout != nil {
		io.WriteString(c.Stdout, f.stdout)
	}
	if c.Stderr != nil {
		io.WriteString(c.Stderr, f.stderr)
	}
	return f.err
}

// useFake installs a backend for one test and puts the previous one back after.
func useFake(t *testing.T, f *fakeBackend) *fakeBackend {
	t.Helper()
	prev := chosen
	Use(f)
	t.Cleanup(func() { Use(prev) })
	return f
}

func TestOutputReturnsStdoutAndTheCommandItRan(t *testing.T) {
	f := useFake(t, &fakeBackend{stdout: "hello\n"})
	tgt := Target{User: "crm", Addr: "1.2.3.4", Port: 22}

	out, err := tgt.Output("tmux", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello\n" {
		t.Errorf("Output = %q, want the backend's stdout", out)
	}
	if f.target != tgt {
		t.Errorf("target = %+v, want %+v", f.target, tgt)
	}
	if got := f.cmd.line(); got != "tmux ls" {
		t.Errorf("remote command = %q, want %q", got, "tmux ls")
	}
}

func TestPipeFeedsStdinAndKeepsTheOutputStreamsApart(t *testing.T) {
	f := useFake(t, &fakeBackend{stdout: "out", stderr: "err"})
	var out, errOut bytes.Buffer

	err := Target{User: "u", Addr: "h"}.Pipe(strings.NewReader("script"), &out, &errOut, "bash -s")
	if err != nil {
		t.Fatal(err)
	}
	if f.stdin != "script" {
		t.Errorf("stdin = %q, want the reader's contents", f.stdin)
	}
	if out.String() != "out" || errOut.String() != "err" {
		t.Errorf("streams crossed: stdout %q, stderr %q", out.String(), errOut.String())
	}
}

// The exit code is the whole reason the transport has an error type of its own:
// the file browser's remote snippets say *why* they failed with one, and that
// has to keep meaning the same thing when a different client runs them.
func TestANonZeroExitIsReportedWithItsCode(t *testing.T) {
	useFake(t, &fakeBackend{err: &ExitError{Code: 7, Err: errors.New("exit status 7")}})

	_, err := Target{User: "u", Addr: "h"}.Output("test", "-d", "nope")

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if exit.ExitCode() != 7 {
		t.Errorf("ExitCode() = %d, want 7", exit.ExitCode())
	}
	if exit.Error() != "exit status 7" {
		t.Errorf("Error() = %q, want the wrapped error's own message", exit.Error())
	}
}

// A caller that reads exit codes must not have to know which backend ran the
// command, so the exec'd one reports them the same way — and an error that is
// not an exit status at all (no ssh binary) stays exactly what it was.
func TestTheExecBackendReportsExitCodesThroughTheSameType(t *testing.T) {
	err := asExitError(exec.Command("sh", "-c", "exit 7").Run())

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if exit.ExitCode() != 7 {
		t.Errorf("ExitCode() = %d, want 7", exit.ExitCode())
	}

	notAnExit := fmt.Errorf("exec: %q: executable file not found in $PATH", "ssh")
	if got := asExitError(notAnExit); got != notAnExit {
		t.Errorf("asExitError rewrote a non-exit failure: %v", got)
	}
	if asExitError(nil) != nil {
		t.Error("asExitError turned success into a failure")
	}
}

// Which backend is in use is a decision, not a preference: nothing changes
// until someone says so, and today that is one environment variable.
func TestTheDefaultBackendIsStillTheSshBinary(t *testing.T) {
	prev := chosen
	Use(nil)
	t.Cleanup(func() { Use(prev) })

	if got := backend().Name(); got != "ssh" {
		t.Errorf("backend with nothing set = %q, want the exec'd ssh", got)
	}
	t.Setenv(backendEnv, "go")
	if got := backend().Name(); got != "go" {
		t.Errorf("backend with %s=go = %q, want the pure-Go client", backendEnv, got)
	}
	t.Setenv(backendEnv, "something-else")
	if got := backend().Name(); got != "ssh" {
		t.Errorf("backend with %s=something-else = %q, want the exec'd ssh", backendEnv, got)
	}

	Use(&fakeBackend{})
	if got := backend().Name(); got != "fake" {
		t.Errorf("an explicitly wired backend was ignored: %q", got)
	}
}

// The seam is only worth having if nothing goes around it: an operation that
// builds an ssh argv has decided which client runs it, and the phone — where
// there is no argv, because there is no process — is exactly where that
// decision cannot be made. Args therefore has one caller, the backend that
// execs ssh, and this is what keeps it that way.
//
// LocalForwardArgs is deliberately not covered: the supervisor's tunnels still
// exec ssh directly, and they move behind this seam in their own step.
func TestNothingBuildsAnSshArgvExceptTheBackendThatExecsIt(t *testing.T) {
	for _, f := range filesUsing(t, ".Args(") {
		if f == "internal/sshx/execssh.go" {
			continue
		}
		t.Errorf("%s builds an ssh argv for a remote command; ask the target for "+
			"Output or Pipe instead, so a client with no argv can run it too", f)
	}
}

// The same rule for the interactive argv, which terminals no longer build: a
// front end asks the target to Open a Shell, and only the exec'd backend turns
// that into `ssh -t`. What is left is the sessions that hand over the terminal
// the *caller* is sitting at, which exist only in the CLI — a program that by
// definition has a shell to be typed into, and therefore an ssh binary to run
// (see RunInteractiveTo).
func TestOnlyTheCLIsOwnSessionsStillBuildAnInteractiveArgv(t *testing.T) {
	allowed := map[string]bool{
		"internal/sshx/sshx.go": true, // where a Shell becomes one
		"forge/shell.go":        true, // workspace ssh, claude attach, expose
		"forge/hosts.go":        true, // host shell
	}
	for _, f := range filesUsing(t, ".TTYArgs(") {
		if allowed[f] {
			continue
		}
		t.Errorf("%s builds an interactive ssh argv; a terminal for a front end comes "+
			"from Open(Shell{...}), so a client with no argv can give it one too", f)
	}
}

// filesUsing lists the repository's non-test Go files that mention needle, as
// paths relative to the module root. It fails the test when nothing does: a rule
// that has stopped matching anything passes for a rule that is satisfied.
func filesUsing(t *testing.T, needle string) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "bin") {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(needle)) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatalf("nothing in the repository uses %s — this check has stopped matching "+
			"what it was written for", needle)
	}
	return found
}

// The two backends must put the same string on the wire, or a command that
// works today breaks when the client changes underneath it. ssh joins its
// trailing arguments with single spaces and lets the login shell parse the
// result; so does the Go client, and this is the test that says they agree —
// for a terminal's command as much as for a command's, since the Claude
// terminal is a tmux attach either way.
func TestBothBackendsSendTheSameCommandString(t *testing.T) {
	remote := []string{"tmux", "paste-buffer", "-d", "-b", "forgecp", "-t", "claude"}
	tgt := Target{User: "crm", Addr: "h", Port: 22}

	argv := tgt.Args(remote...)
	execLine := strings.Join(argv[len(argv)-len(remote):], " ")
	goLine := Command{Remote: remote}.line()

	if execLine != goLine {
		t.Errorf("the backends disagree on the command:\n exec: %q\n   go: %q", execLine, goLine)
	}
	if goLine != "tmux paste-buffer -d -b forgecp -t claude" {
		t.Errorf("unexpected command line: %q", goLine)
	}

	term := Shell{Remote: remote}
	termArgv := tgt.ttyArgs(term)
	execTermLine := strings.Join(termArgv[len(termArgv)-len(remote):], " ")
	if execTermLine != term.line() {
		t.Errorf("the backends disagree on a terminal's command:\n exec: %q\n   go: %q",
			execTermLine, term.line())
	}
}

// Opening a terminal goes through the same chosen backend as a command, with the
// target and the kind's own two decisions — what to run, and whether the agent
// goes along — handed over untouched.
func TestOpenHandsTheTargetAndTheShellToTheBackend(t *testing.T) {
	f := useFake(t, &fakeBackend{})
	tgt := Target{User: "crm", Addr: "1.2.3.4", Port: 2222}

	term, err := tgt.Open(Shell{Remote: []string{"tmux", "attach"}, Cols: 100, Rows: 30, ForwardAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if f.target != tgt {
		t.Errorf("target = %+v, want %+v", f.target, tgt)
	}
	if got := f.shell.line(); got != "tmux attach" {
		t.Errorf("terminal command = %q, want %q", got, "tmux attach")
	}
	if f.shell.Cols != 100 || f.shell.Rows != 30 {
		t.Errorf("size = %dx%d, want 100x30", f.shell.Cols, f.shell.Rows)
	}
	if !f.shell.ForwardAgent {
		t.Error("the request to forward the agent was dropped on the way to the backend")
	}
}

// A caller with no size must not get whatever a pty happens to default to — 0×0
// on Linux, which is a rectangle nothing can draw into. Both backends answer it
// from here, so an unsized terminal is the same shape whichever client opened it:
// the Go one asks for these numbers in its pty-req, the exec'd one puts them on
// the pty it hands ssh.
func TestATerminalWithNoSizeOpensAtTheConventionalOne(t *testing.T) {
	if cols, rows := (Shell{}).size(); cols != 80 || rows != 24 {
		t.Errorf("an unsized terminal opens at %dx%d, want 80x24", cols, rows)
	}
	// Half a size is no size: a 100×0 terminal is as unusable as a 0×0 one.
	if cols, rows := (Shell{Cols: 100}).size(); cols != 80 || rows != 24 {
		t.Errorf("a terminal with no rows opens at %dx%d, want 80x24", cols, rows)
	}
	// And a size that was given is passed through untouched.
	if cols, rows := (Shell{Cols: 100, Rows: 30}).size(); cols != 100 || rows != 30 {
		t.Errorf("size = %dx%d, wanted the 100x30 asked for", cols, rows)
	}
}

// The exec'd backend's terminal argv is the one Forge has always run: the UI used
// to build it itself, and moving it here must not have changed a word of it —
// same forced TTY, same options, same order, and -A where the workspace shell has
// always had it.
func TestTheExecdTerminalArgvIsTheOneForgeAlwaysRan(t *testing.T) {
	tgt := Target{User: "crm", Addr: "203.0.113.7", Port: 2222}

	shell := tgt.ttyArgs(Shell{ForwardAgent: true, Cols: 80, Rows: 24})
	claude := tgt.ttyArgs(Shell{Remote: []string{"tmux", "attach"}})
	host := tgt.ttyArgs(Shell{})

	// -t on every one of them: without it tmux refuses to attach and a shell has
	// no line editing.
	for name, args := range map[string][]string{"shell": shell, "claude": claude, "host": host} {
		if !slices.Contains(args, "-t") {
			t.Errorf("the %s terminal does not force a TTY: %v", name, args)
		}
		if i := slices.Index(args, "-p"); i < 0 || args[i+1] != "2222" {
			t.Errorf("the %s terminal ignores the host's port: %v", name, args)
		}
	}

	// -A comes before everything, which is where it was when the UI wrote this
	// argv by hand.
	if shell[0] != "-A" || shell[1] != "-t" {
		t.Errorf("agent forwarding is not where it was: %v", shell)
	}
	if slices.Contains(host, "-A") {
		t.Errorf("a terminal that did not ask for the agent forwards it: %v", host)
	}

	// The command, if any, is the tail — and a terminal without one ends at the
	// destination, so ssh gives it a login shell.
	if last := len(claude); claude[last-2] != "tmux" || claude[last-1] != "attach" {
		t.Errorf("the command is not the tail of the argv: %v", claude)
	}
	if last := host[len(host)-1]; last != "crm@203.0.113.7" {
		t.Errorf("the argv ends at %q, want just the destination", last)
	}
}
