package cli

import (
	"reflect"
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
	want := []WorkspaceStatus{{Name: "mine", Host: "box", Status: agentproto.StatusRunning}}
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
	want := []WorkspaceStatus{
		{Name: "alpha", Host: "a", Status: agentproto.StatusRunning},
		{Name: "zeta", Host: "b", Status: agentproto.StatusStopped},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
