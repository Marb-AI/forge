package forge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The key is made once and read many times: you come back for the public half
// every time you add a server, and finding a NEW key each time would mean every
// server you already have stops letting this device in.
func TestSetupMakesOneKeyAndKeepsShowingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)

	first, created, err := Setup()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first run reported that it made nothing")
	}
	if !strings.HasPrefix(first, "ssh-ed25519 ") {
		t.Errorf("public half = %q, want an authorized_keys line", first)
	}
	if _, err := os.Stat(filepath.Join(dir, "id.pem")); err != nil {
		t.Errorf("no key where the core was pointed: %v", err)
	}

	again, created, err := Setup()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the second run made another key — every server trusting the first would stop")
	}
	if again != first {
		t.Errorf("the line changed between runs:\n%q\n%q", first, again)
	}
}

// Nothing makes a key as a side effect of needing one. A pair that appears
// because something wanted to read the public half is a pair no server has been
// told about, on a device that was working a moment ago.
func TestOnlySetupCreatesTheKey(t *testing.T) {
	for _, f := range grepRepo(t, ".Create()") {
		if f == "forge/setup.go" {
			continue
		}
		t.Errorf("%s creates this device's key; that belongs to forge setup, which is "+
			"the one place somebody asked for it", f)
	}
}
