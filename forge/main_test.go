package forge

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, before any
// test runs.
//
// Every operation here starts by reading the store, and a test that does not say
// otherwise gets the default one: ~/.forge, resolved from HOME. (A test that wants
// its own directory says so with Open, and state_test.go does.) Isolating for the
// package rather than per test is the point: per-test t.Setenv protects only the
// tests that remember to call it, and the one that forgets writes to the
// developer's own machine — which is how this package's predecessor once replaced
// a real config's forwards with fixture data. There is nothing to remember now.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-core-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
