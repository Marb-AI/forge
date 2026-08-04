package forge

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// The host's own list is every directory under /home/workspaces — including ones
// Forge never created: a colleague's, or one made by hand. They are not ours, and
// every command refuses to touch a workspace that isn't in our config. Listing
// them offers exactly what we will then decline to do.
func TestListShowsOnlyOurOwnWorkspaces(t *testing.T) {
	mine := map[string]string{"mine": "box"}
	onTheHost := map[string]map[string]string{
		"box": {
			"mine":          agentproto.StatusRunning,
			"someone-elses": agentproto.StatusRunning, // created from another laptop
			"made-by-hand":  agentproto.StatusStopped, // never went through forge
		},
	}

	got := mergeWorkspaceStatus(mine, onTheHost)
	want := []WorkspaceInfo{{Name: "mine", Host: "box", Status: agentproto.StatusRunning}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listing must be ours alone.\n got: %+v\nwant: %+v", got, want)
	}
}

// The other direction: our config claims a workspace the host doesn't have. It was
// deleted from another machine. "stopped" would be a lie you could act on — there
// is nothing left to start.
func TestWorkspaceDeletedElsewhereIsMissing(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"gone": "box"},
		map[string]map[string]string{"box": {}}, // the host answered, and doesn't have it
	)
	if len(got) != 1 || got[0].Status != agentproto.StatusMissing {
		t.Errorf("a workspace the host no longer has must read as missing, got %+v", got)
	}
}

// And an unreachable host is not the same as a stopped session: we simply don't
// know, and saying "stopped" would invite you to press Start against a box we
// can't even reach.
func TestUnreachableHostIsNotStopped(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"ws": "box"},
		map[string]map[string]string{}, // nobody answered
	)
	if len(got) != 1 || got[0].Status != agentproto.StatusUnreachable {
		t.Errorf("a workspace on an unreachable host must say so, got %+v", got)
	}
}

func TestListIsSortedAndKeepsItsHost(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"zeta": "b", "alpha": "a"},
		map[string]map[string]string{
			"a": {"alpha": agentproto.StatusRunning},
			"b": {"zeta": agentproto.StatusStopped},
		},
	)
	want := []WorkspaceInfo{
		{Name: "alpha", Host: "a", Status: agentproto.StatusRunning},
		{Name: "zeta", Host: "b", Status: agentproto.StatusStopped},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A workspace has to let in the key Forge connects with, or it is one this
// client can create and then never open — and after the transport flips there is
// no second key to fall back on.
func TestANewWorkspaceLetsInThisDevice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)

	mine, _, err := Setup()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := workspaceKeys()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(keys)); got != strings.TrimSpace(mine) {
		t.Errorf("authorized_keys would hold %q, want this device's key %q", got, mine)
	}
}

// FORGE_PUBKEY is for reaching a workspace from a plain terminal, without Forge
// in the middle. It sits BESIDE the device key rather than replacing it: as a
// replacement, one environment variable could produce a workspace this client
// cannot enter, and nothing would say so until the first attempt.
func TestAnExtraKeyJoinsTheDeviceKeyRatherThanReplacingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)
	mine, _, err := Setup()
	if err != nil {
		t.Fatal(err)
	}

	theirs := filepath.Join(t.TempDir(), "other.pub")
	if err := os.WriteFile(theirs, []byte("ssh-ed25519 AAAAsomebodyelse them@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_PUBKEY", theirs)

	keys, err := workspaceKeys()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(keys)), "\n")
	if len(lines) != 2 {
		t.Fatalf("authorized_keys holds %d keys, want both: %q", len(lines), keys)
	}
	if lines[0] != strings.TrimSpace(mine) {
		t.Errorf("first line = %q, want this device's key", lines[0])
	}
	if !strings.Contains(lines[1], "somebodyelse") {
		t.Errorf("second line = %q, want the one FORGE_PUBKEY named", lines[1])
	}
}

// And a device with no key does not get to create a workspace nobody can enter.
func TestCreatingAWorkspaceNeedsAKeyFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)

	_, err := workspaceKeys()
	if err == nil {
		t.Fatal("a workspace would have been created with no key to enter it by")
	}
	if !strings.Contains(err.Error(), "forge setup") {
		t.Errorf("err = %v, want it to name the command that makes one", err)
	}
}

// A path out of an environment variable goes in the error quoted. Paths with
// spaces are ordinary — "/Users/me/My Keys/id.pub" unquoted reads as a path that
// ends at "My", and the reader is left guessing where the message begins.
func TestAnUnreadableExtraKeyNamesThePathUnambiguously(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	swapState(t, dir)
	if _, _, err := Setup(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_PUBKEY", filepath.Join(t.TempDir(), "My Keys", "absent.pub"))

	_, err := workspaceKeys()
	if err == nil {
		t.Fatal("a key file that is not there was accepted")
	}
	if !strings.Contains(err.Error(), `"`) {
		t.Errorf("err = %v, want the path quoted so its ends are visible", err)
	}
}
