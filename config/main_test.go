package config

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, before any
// test runs.
//
// The tests here name their own directories now — a store is handed one, which is
// the point of the seam — so nothing should reach for HOME at all. This is the
// backstop for that "should": a test that resolves the default location and then
// writes to it would otherwise land in the developer's own ~/.forge, which is how
// this repository once replaced a real config's forwards with fixture data. The
// isolation is for the package rather than per test, because a per-test t.Setenv
// only protects the tests that remember it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-config-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
