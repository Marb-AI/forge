package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchHost points both directories this file touches at a temp tree: the
// workspaces themselves, and the root-owned directory the record lives in.
func scratchHost(t *testing.T, workspaces ...string) string {
	t.Helper()
	root := t.TempDir()

	prevBase, prevKeys := baseDir, hostKeyDir
	baseDir = filepath.Join(root, "home", "workspaces")
	hostKeyDir = filepath.Join(root, "etc", "forge")
	t.Cleanup(func() { baseDir, hostKeyDir = prevBase, prevKeys })

	for _, name := range workspaces {
		if err := os.MkdirAll(filepath.Join(baseDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A host that has never been told records nothing, and that is not a failure.
//
// It is every host that existed before this: the client's own config is still
// the answer there, and nothing about reading the record may turn an old server
// into a broken one.
func TestAHostThatWasNeverToldRecordsNothing(t *testing.T) {
	scratchHost(t, "one", "two")

	r, err := readRoster()
	if err != nil {
		t.Fatalf("reading a record that does not exist: %v", err)
	}
	if len(r.Workspaces) != 0 {
		t.Errorf("a host with no record claims %v", r.Workspaces)
	}
}

// Adopting is the migration, and the only way a workspace older than the record
// gets into it: the host cannot tell its own accounts apart, so the client that
// made them says which are its.
func TestAdoptingRecordsAWorkspaceTheHostAlreadyHas(t *testing.T) {
	scratchHost(t, "ours", "somebody-elses")

	if code := opAdopt([]string{"-name", "ours"}); code != 0 {
		t.Fatalf("adopting an existing workspace exited %d", code)
	}
	r, _ := readRoster()
	if len(r.Workspaces) != 1 || r.Workspaces[0] != "ours" {
		t.Errorf("the record holds %v, want just [ours]", r.Workspaces)
	}

	// Said twice is said once: a client repeating what it already believes, or a
	// creation retried after a failure further down.
	if code := opAdopt([]string{"-name", "ours"}); code != 0 {
		t.Fatalf("adopting twice exited %d", code)
	}
	if r, _ := readRoster(); len(r.Workspaces) != 1 {
		t.Errorf("adopting twice recorded %v", r.Workspaces)
	}
}

// A record naming accounts that do not exist is worse than an empty one,
// because it would be believed: Forge would open shells and tunnel ports for
// something that is not there — or worse, for whatever takes the name later.
func TestAdoptingSomethingThatIsNotThereIsRefused(t *testing.T) {
	scratchHost(t, "real")

	out := captureStdout(t)
	code := opAdopt([]string{"-name", "imaginary"})
	said := out()

	if code == 0 {
		t.Error("adopted a workspace this host does not have")
	}
	if !strings.Contains(said, "no workspace") {
		t.Errorf("the refusal does not say why: %s", said)
	}
	if r, _ := readRoster(); len(r.Workspaces) != 0 {
		t.Errorf("it was recorded anyway: %v", r.Workspaces)
	}
}

// The listing says which of the host's accounts are Forge's, which is the whole
// point: a second device asks the host rather than a laptop's config file.
func TestTheListingSaysWhichWorkspacesAreOurs(t *testing.T) {
	scratchHost(t, "ours", "somebody-elses")
	if err := recordWorkspace("ours"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t)
	if code := opList(); code != 0 {
		t.Fatalf("list exited %d", code)
	}
	got := out()

	// Both are listed — the host's directory is what it is — and exactly one is
	// claimed. A listing that dropped the others would hide a name collision from
	// whoever is about to create a workspace.
	for _, name := range []string{"ours", "somebody-elses"} {
		if !strings.Contains(got, `"name":"`+name+`"`) {
			t.Errorf("%q is missing from the listing:\n%s", name, got)
		}
	}
	if n := strings.Count(got, `"ours":true`); n != 1 {
		t.Errorf("%d workspaces are claimed, want 1:\n%s", n, got)
	}
}

// Forgetting is idempotent for the same reason adopting is, and a name that was
// never there is a name already forgotten.
func TestForgettingAWorkspaceThatWasNeverRecorded(t *testing.T) {
	scratchHost(t, "one")
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	if err := forgetWorkspace("never-here"); err != nil {
		t.Fatalf("forgetting something unrecorded: %v", err)
	}
	if r, _ := readRoster(); len(r.Workspaces) != 1 {
		t.Errorf("it took something else with it: %v", r.Workspaces)
	}
}

// Nothing but root may read or write the record.
//
// This is a boundary rather than tidiness: a workspace user who could add a name
// to it would have Forge treat any account on the box as a workspace of its own
// — open a shell as it, tunnel its ports, hand it a Claude session.
func TestTheRecordIsRootsAlone(t *testing.T) {
	scratchHost(t, "one")
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(rosterPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the record is mode %04o — anything on the host can read it, and "+
			"a group-writable one could name accounts Forge would then own", perm)
	}
	// And it is in the directory `host prepare` already owns, not somewhere a
	// workspace user's home could reach.
	if !strings.HasPrefix(rosterPath(), hostKeyDir) {
		t.Errorf("the record is at %q, outside %q", rosterPath(), hostKeyDir)
	}
}

// The file is replaced by rename, so a reader never sees half of one. Two agents
// run at once whenever a client creates a workspace while another deletes one:
// two SSH sessions, two processes, one file.
func TestTheRecordIsReplacedWhole(t *testing.T) {
	scratchHost(t, "one", "two")
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	if err := recordWorkspace("two"); err != nil {
		t.Fatal(err)
	}

	// Nothing partial left behind, which is what a write straight to the path
	// would leave on a full disk or a killed process.
	if _, err := os.Stat(rosterPath() + ".new"); !os.IsNotExist(err) {
		t.Error("a temporary file survived the write")
	}
	r, err := readRoster()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Workspaces) != 2 || r.Workspaces[0] != "one" || r.Workspaces[1] != "two" {
		t.Errorf("the record reads %v, want [one two] in order", r.Workspaces)
	}
}
