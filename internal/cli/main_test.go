package cli

import (
	"os"
	"testing"
)

// TestMain points HOME at a throwaway directory for the whole package.
//
// The commands here no longer write anything themselves, but they call
// operations that do — a routing test that runs `host add` writes a config, and
// it must never be the developer's. Isolating for the package rather than per
// test is the point: per-test t.Setenv protects only the tests that remember it,
// and the one that forgets writes to a real ~/.forge.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-cli-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir) // os.UserHomeDir reads this one on Windows
	code := m.Run()
	os.RemoveAll(dir) // not deferred: os.Exit does not run defers
	os.Exit(code)
}
