package agent

import (
	"fmt"
	"os"
	"testing"
)

// stubRun stands in for the host for one test, and hands back what was asked of
// it in the order it was asked.
func stubRun(t *testing.T, fn func(name string, args ...string) (string, error)) *[][]string {
	t.Helper()
	var seen [][]string
	prev := run
	run = func(name string, args ...string) (string, error) {
		seen = append(seen, append([]string{name}, args...))
		if fn != nil {
			return fn(name, args...)
		}
		return "", nil
	}
	t.Cleanup(func() { run = prev })
	return &seen
}

// TestMain points HOME at a throwaway directory for the whole package, before
// any test runs.
//
// This package writes to home directories: authorize-key appends to the admin
// login's authorized_keys, and the admin login is whichever account the agent
// is running as. On a server that is a machine nobody develops on. Under `go
// test` it is the developer's own — and the first version of authorize.go read
// user.Current().HomeDir, which ignores HOME, so setting it per test achieved
// nothing at all: the tests wrote a fabricated key into the real
// ~/.ssh/authorized_keys of the machine running them.
//
// Isolating here rather than in each test is the point, and the same one
// internal/supervisor's TestMain makes: per-test t.Setenv works only for the
// tests that remember, and the one that forgets writes to a real file on a real
// machine. There is nothing to remember now — and the code under test honours
// HOME, which is the other half of it.
// It also takes the host away. Every command this package runs goes through
// `run`, and here that becomes something which refuses and says which command
// was attempted — so a test that reaches for useradd fails on the spot instead
// of passing because the developer's machine happens not to have it.
//
// A test that genuinely needs to watch a command calls stubRun. Nothing can
// touch the host by accident any more, which matters more now than it did:
// these run in a container, on Linux, where every one of them would work.
func TestMain(m *testing.M) {
	run = func(name string, args ...string) (string, error) {
		return "", fmt.Errorf("this test ran %q on the host; stub it with stubRun "+
			"if the test needs to watch commands", name)
	}

	dir, err := os.MkdirTemp("", "forge-agent-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	os.Unsetenv("SUDO_USER")      // these tests are not running under sudo
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
