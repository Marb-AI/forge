package forge

import (
	"errors"
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

	got := mergeWorkspaceStatus(mine, fromOldHosts(onTheHost))
	want := []WorkspaceInfo{{Name: "mine", Host: "box", Status: agentproto.StatusRunning}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listing must be ours alone.\n got: %+v\nwant: %+v", got, want)
	}
}

// fromOldHosts turns "here is what each host reported" into an answer from a
// server that keeps no record of its own — which is every server that predates
// one, and the case where this client's config is still the authority.
func fromOldHosts(sessions map[string]map[string]string) map[string]hostWorkspaces {
	out := map[string]hostWorkspaces{}
	for alias, byName := range sessions {
		out[alias] = hostWorkspaces{sessions: byName, ours: map[string]bool{}}
	}
	return out
}

// The other direction: our config claims a workspace the host doesn't have. It was
// deleted from another machine. "stopped" would be a lie you could act on — there
// is nothing left to start.
func TestWorkspaceDeletedElsewhereIsMissing(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"gone": "box"},
		fromOldHosts(map[string]map[string]string{"box": {}}), // answered, and doesn't have it
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
		fromOldHosts(nil), // nobody answered
	)
	if len(got) != 1 || got[0].Status != agentproto.StatusUnreachable {
		t.Errorf("a workspace on an unreachable host must say so, got %+v", got)
	}
}

func TestListIsSortedAndKeepsItsHost(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"zeta": "b", "alpha": "a"},
		fromOldHosts(map[string]map[string]string{
			"a": {"alpha": agentproto.StatusRunning},
			"b": {"zeta": agentproto.StatusStopped},
		}),
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

// A host that keeps its own record is the authority on its own workspaces.
//
// This is the whole of what makes a second device possible: the phone has never
// heard of these names, and there is nowhere for it to have heard them from
// except the machine they are on.
func TestAHostThatKeepsARecordIsBelievedOverThisDevice(t *testing.T) {
	// A device that knows nothing — a phone, or a laptop restored from nothing.
	got := mergeWorkspaceStatus(map[string]string{}, map[string]hostWorkspaces{
		"box": {
			recorded: true,
			ours:     map[string]bool{"api": true, "web": true},
			sessions: map[string]string{
				"api":           agentproto.StatusRunning,
				"web":           agentproto.StatusStopped,
				"made-by-hand":  agentproto.StatusRunning, // never went through forge
				"someone-elses": agentproto.StatusStopped,
			},
		},
	})

	want := []WorkspaceInfo{
		{Name: "api", Host: "box", Status: agentproto.StatusRunning},
		{Name: "web", Host: "box", Status: agentproto.StatusStopped},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a device with an empty config saw:\n %+v\nwant the host's own two:\n %+v",
			got, want)
	}
}

// And it is believed the other way too: a workspace this config still claims,
// which the host says is not Forge's, has been deleted from another device.
// Keeping it would mean every device that ever saw a workspace shows it forever.
func TestAHostWithARecordAlsoSettlesWhatIsGone(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"old": "box", "still-here": "box"},
		map[string]hostWorkspaces{
			"box": {
				recorded: true,
				ours:     map[string]bool{"still-here": true},
				sessions: map[string]string{"still-here": agentproto.StatusRunning},
			},
		})

	if len(got) != 1 || got[0].Name != "still-here" {
		t.Errorf("got %+v, want only the one the host still claims", got)
	}
}

// A server from before the record is not a server that owns nothing.
//
// It answers with nothing claimed, which on the wire is what a host with a
// record and no workspaces looks like. Reading them alike would take a working
// laptop and empty it — every workspace on every server not yet updated.
func TestAnOldHostLeavesThisDeviceAsTheAuthority(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"mine": "box"},
		map[string]hostWorkspaces{
			"box": {
				recorded: false, // an agent that never heard of the record
				ours:     map[string]bool{},
				sessions: map[string]string{"mine": agentproto.StatusRunning},
			},
		})

	want := []WorkspaceInfo{{Name: "mine", Host: "box", Status: agentproto.StatusRunning}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("an old host emptied the list:\n got: %+v\nwant: %+v", got, want)
	}
}

// A host that is off says nothing at all, which is not the same as saying no.
// Its workspaces stay listed, as unreachable — a server being down must not make
// work disappear from the screen.
func TestAHostThatIsOffKeepsItsWorkspacesListed(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"mine": "box"},
		map[string]hostWorkspaces{}, // nobody answered
	)
	if len(got) != 1 || got[0].Status != agentproto.StatusUnreachable {
		t.Errorf("got %+v, want the workspace listed as unreachable", got)
	}
}

// Two servers, one of each kind, in one listing: the updated host answers for
// itself and the old one leaves this config to answer for it.
func TestOneUpdatedHostAndOneOldOneInTheSameList(t *testing.T) {
	got := mergeWorkspaceStatus(
		map[string]string{"legacy": "old"},
		map[string]hostWorkspaces{
			"new": {
				recorded: true,
				ours:     map[string]bool{"fresh": true},
				sessions: map[string]string{"fresh": agentproto.StatusRunning},
			},
			"old": {
				recorded: false,
				ours:     map[string]bool{},
				sessions: map[string]string{"legacy": agentproto.StatusStopped},
			},
		})

	want := []WorkspaceInfo{
		{Name: "fresh", Host: "new", Status: agentproto.StatusRunning},
		{Name: "legacy", Host: "old", Status: agentproto.StatusStopped},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A host that could not be reached and an agent too old to know the command both
// name zero workspaces, and they want different things done about them: one
// wants switching on, the other wants `forge host update`. A single count could
// not tell them apart, which is why this is a struct.
func TestAdoptingTellsAnOldAgentFromAnAbsentHost(t *testing.T) {
	old := Adopted{Err: errors.New(`agent: unknown op "workspace-adopt"`)}
	if !old.TooOld() {
		t.Error("an agent that does not know the command is not recognised as old")
	}

	off := Adopted{Err: errors.New("ssh/forge-agent failed: dial tcp: connection refused")}
	if off.TooOld() {
		t.Error("a server that is off was reported as an old agent, which sends " +
			"somebody to update a machine that is not answering")
	}

	if (Adopted{Named: 3}).TooOld() {
		t.Error("a host that worked was reported as too old")
	}
}
