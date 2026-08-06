package agent

import (
	"os"
	"testing"
)

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
func TestMain(m *testing.M) {
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
