package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A host with a record and nothing in it is not a host without one, and the two
// mean opposite things: an empty record says "Forge owns nothing here", while no
// record says "this host cannot answer — ask your own config, as you always
// have". An old agent produces the second, and a client that read them alike
// would look at a working server and hide every workspace on it.
func TestAnEmptyRecordIsNotTheSameAsNoRecord(t *testing.T) {
	scratchHost(t, "one")

	out := captureStdout(t)
	if code := opList(); code != 0 {
		t.Fatalf("list exited %d", code)
	}
	if got := out(); !strings.Contains(got, `"recorded":false`) {
		t.Errorf("a host that was never told says it keeps a record:\n%s", got)
	}

	// Recording and then forgetting leaves the file behind, empty — which is a
	// host that keeps a record and owns nothing, and must say so.
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	if err := forgetWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t)
	if code := opList(); code != 0 {
		t.Fatalf("list exited %d", code)
	}
	got := out()
	if !strings.Contains(got, `"recorded":true`) {
		t.Errorf("a host with an empty record says it has none:\n%s", got)
	}
	if strings.Contains(got, `"ours":true`) {
		t.Errorf("it claims something after forgetting everything:\n%s", got)
	}
	// And "ours" is on the wire even when false, or "not ours" and "an agent that
	// never heard of this" would look the same again.
	if !strings.Contains(got, `"ours":false`) {
		t.Errorf("a workspace that is not ours says nothing at all:\n%s", got)
	}
}

// Two agents recording at the same time both survive. Nothing atomic about the
// write makes that true — each reads, changes its copy and writes it back, so
// without a lock across processes one of the two workspaces is simply gone.
func TestTwoAgentsRecordingAtOnceDoNotLoseEachOther(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	scratchHost(t, names...)

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if err := recordWorkspace(name); err != nil {
				t.Errorf("recording %q: %v", name, err)
			}
		}(name)
	}
	wg.Wait()

	r, err := readRoster()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Workspaces) != len(names) {
		t.Errorf("the record holds %d of %d workspaces: %v — writes were lost",
			len(r.Workspaces), len(names), r.Workspaces)
	}
	// And nothing half-written was left where the next read would find it.
	entries, _ := os.ReadDir(hostKeyDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "workspaces-") {
			t.Errorf("a temporary file survived: %s", e.Name())
		}
	}
}

// Which ports a machine hands out is a fact about the machine.
//
// A second device that guessed would hand out a block the first had already
// given away — and the tunnel for it is -L port:localhost:port, so the two would
// then fight over the same local number and one of them would simply not work.
func TestTheHostRemembersItsPortRange(t *testing.T) {
	scratchHost(t)

	out := captureStdout(t)
	if code := opPortRange([]string{"-start", "16000", "-end", "30000", "-block", "100"}); code != 0 {
		t.Fatalf("setting the range exited %d", code)
	}
	if got := out(); !strings.Contains(got, `"start":16000`) || !strings.Contains(got, `"set":true`) {
		t.Errorf("setting the range did not answer with it:\n%s", got)
	}

	// And it is there for whoever asks next, which is the point: a device that has
	// never seen this machine.
	out = captureStdout(t)
	if code := opPortRange(nil); code != 0 {
		t.Fatalf("reading the range exited %d", code)
	}
	got := out()
	for _, want := range []string{`"start":16000`, `"end":30000`, `"block":100`, `"set":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("the range came back without %s:\n%s", want, got)
		}
	}
}

// A host that keeps a record and has never been told its range is not a host
// that hands out nothing. The two absences are different questions and a client
// answers them differently: one says "ask your own config", the other says "this
// machine is waiting to be told".
func TestNotYetToldIsNotTheSameAsAnEmptyRange(t *testing.T) {
	scratchHost(t)

	// No record at all.
	out := captureStdout(t)
	opPortRange(nil)
	got := out()
	if !strings.Contains(got, `"recorded":false`) || !strings.Contains(got, `"set":false`) {
		t.Errorf("a host that was never told says something else:\n%s", got)
	}

	// A record, and still no range: recording a workspace creates the file.
	if err := recordWorkspace("ws"); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t)
	opPortRange(nil)
	got = out()
	if !strings.Contains(got, `"recorded":true`) {
		t.Errorf("a host with a record says it has none:\n%s", got)
	}
	if !strings.Contains(got, `"set":false`) {
		t.Errorf("a host with no range says it has one:\n%s", got)
	}
}

// A range with a zero in it is not a range: it would hand out blocks starting at
// port 0. Half the flags is a caller that means one thing and typed another.
func TestAPortRangeIsRefusedUnlessItIsOne(t *testing.T) {
	for _, args := range [][]string{
		{"-start", "16000"},                                   // and nothing else
		{"-start", "16000", "-end", "30000"},                  // no block
		{"-start", "16000", "-end", "15000", "-block", "100"}, // ends before it begins
		{"-start", "16000", "-end", "16050", "-block", "100"}, // a block does not fit
		{"-start", "-1", "-end", "30000", "-block", "100"},    // before the first port
	} {
		scratchHost(t)
		out := captureStdout(t)
		code := opPortRange(args)
		said := out()

		if code == 0 {
			t.Errorf("%v was accepted as a port range", args)
		}
		if !strings.Contains(said, `"error"`) {
			t.Errorf("%v was refused without saying why: %s", args, said)
		}
		if r, _ := readRoster(); r.PortRange != nil {
			t.Errorf("%v was recorded anyway: %+v", args, r.PortRange)
		}
	}
}

// The range shares a file with the workspace list, so setting one while another
// agent records a workspace is the read-modify-write that must not lose either.
func TestSettingTheRangeDoesNotLoseWorkspaces(t *testing.T) {
	scratchHost(t, "a", "b", "c", "d")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		opPortRange([]string{"-start", "16000", "-end", "30000", "-block", "100"})
	}()
	for _, name := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if err := recordWorkspace(name); err != nil {
				t.Errorf("recording %q: %v", name, err)
			}
		}(name)
	}
	wg.Wait()

	r, err := readRoster()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Workspaces) != 4 {
		t.Errorf("the record holds %v, want all four", r.Workspaces)
	}
	if r.PortRange == nil || r.PortRange.Start != 16000 {
		t.Errorf("the range is %+v, want the one that was set", r.PortRange)
	}
}
