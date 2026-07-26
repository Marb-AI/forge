package ui

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, before any
// test runs.
//
// The handlers here reach nothing but their fakes, and the tests that start a
// real server name their own directory — Start is handed one, which is the point
// of it. This is the backstop for that: this package can now start the core, and
// a core that is not told where to look answers ~/.forge, resolved from HOME. A
// test that forgot would write to the developer's own Forge, on a machine where
// it is running. Isolating for the package rather than per test is deliberate: a
// per-test t.Setenv protects only the tests that remember it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-ui-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
