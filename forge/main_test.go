package forge

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package, before any
// test runs.
//
// Every operation here starts with config.Load, which resolves ~/.forge from HOME,
// and the writes land in this package next. Isolating for the package rather than
// per test is the point: per-test t.Setenv protects only the tests that remember to
// call it, and the one that forgets writes to the developer's own machine — which
// is how this package's predecessor once replaced a real config's forwards with
// fixture data. There is nothing to remember now.
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
