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
	"strings"
	"testing"
)

// fakeBackend records what it was asked to run and answers with what the test
// wants back, so the plumbing above the seam can be checked without a server.
type fakeBackend struct {
	target Target
	cmd    Command
	stdin  string
	stdout string
	stderr string
	err    error
}

func (f *fakeBackend) Name() string { return "fake" }

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
// The interactive argv (TTYArgs, LocalForwardArgs) is deliberately not covered:
// terminals and tunnels still exec ssh directly, each moving behind this seam in
// its own step.
func TestNothingBuildsAnSshArgvExceptTheBackendThatExecsIt(t *testing.T) {
	root := filepath.Join("..", "..")
	var callers []string
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
		if bytes.Contains(data, []byte(".Args(")) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			callers = append(callers, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) == 0 {
		t.Fatal("nothing builds an ssh argv at all — this check has stopped matching what it was written for")
	}
	for _, f := range callers {
		if f == "internal/sshx/execssh.go" {
			continue
		}
		t.Errorf("%s builds an ssh argv for a remote command; ask the target for "+
			"Output or Pipe instead, so a client with no argv can run it too", f)
	}
}

// The two backends must put the same string on the wire, or a command that
// works today breaks when the client changes underneath it. ssh joins its
// trailing arguments with single spaces and lets the login shell parse the
// result; so does the Go client, and this is the test that says they agree.
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
}
