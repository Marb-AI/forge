package supervisor

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, before any
// test runs.
//
// This package writes the real config: observeAll caches what it saw, and cache
// goes through config.Update, which resolves ~/.forge from HOME. A test that
// exercised observation without isolating HOME therefore rewrote the developer's
// own forwards with fixture data — "srv/crm/16000" landing on top of a machine
// with nine live workspaces. It did exactly what it was told; nothing in the test
// said where to write.
//
// Isolating here rather than in each test is the point: per-test t.Setenv works
// only for the tests that remember, and the one that forgets corrupts a real file
// on a real machine. There is nothing to remember now.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-supervisor-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
