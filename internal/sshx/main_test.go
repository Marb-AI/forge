package sshx

import (
	"os"
	"testing"
)

// TestMain gives the package a throwaway HOME and no SSH agent, for the whole
// run, before any test starts.
//
// Both matter here for the same reason: the pure-Go backend authenticates from
// ~/.ssh and an agent socket, so a test that connects would otherwise reach for
// the developer's own keys — offering them to a test server, and writing its
// known_hosts entries into a real ~/.ssh/known_hosts. Isolating for the package
// rather than per test is the point: per-test t.Setenv protects only the tests
// that remember to call it, and the one that forgets is the one that touches a
// real machine.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-sshx-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	os.Unsetenv("SSH_AUTH_SOCK")
	os.Unsetenv(backendEnv)
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
