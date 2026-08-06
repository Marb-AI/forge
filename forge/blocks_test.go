package forge

import (
	"testing"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

// The core's block and the agent's wire block are two spellings of the same run
// of ports, and portBlock is the only place they meet. They have to agree on
// where a block ends: every port Forge prints, links to or tunnels is derived
// from that, so a drift here is silently off-by-N everywhere at once.
func TestPortBlockMatchesTheWireBlock(t *testing.T) {
	wire := &agentproto.PortBlock{Start: 16000, Size: 100}
	got := portBlock(wire)
	if got.Start != wire.Start || got.Size != wire.Size {
		t.Fatalf("portBlock = %+v, want %+v", got, wire)
	}
	if got.End() != wire.End() {
		t.Errorf("End() = %d, the wire block says %d", got.End(), wire.End())
	}
	// No block is a state a workspace can genuinely be in (created before blocks
	// existed), and it has to survive the conversion as itself rather than as a
	// block starting at zero.
	if portBlock(nil) != nil {
		t.Error("a workspace with no block must convert to no block")
	}
}

func TestNextFreeBlock(t *testing.T) {
	r := config.PortRange{Start: 16000, End: 16299, Block: 100}

	if got, ok := nextFreeBlock(r, map[int]bool{}); !ok || got != 16000 {
		t.Errorf("empty = %d, %v; want 16000", got, ok)
	}
	if got, ok := nextFreeBlock(r, map[int]bool{16000: true}); !ok || got != 16100 {
		t.Errorf("one taken = %d, %v; want 16100", got, ok)
	}

	// A hole left by a deleted workspace is reused rather than skipped, so the
	// numbers stay small and memorable instead of drifting up forever.
	if got, ok := nextFreeBlock(r, map[int]bool{16000: true, 16200: true}); !ok || got != 16100 {
		t.Errorf("hole = %d, %v; want 16100", got, ok)
	}

	full := map[int]bool{16000: true, 16100: true, 16200: true}
	if _, ok := nextFreeBlock(r, full); ok {
		t.Error("a full range should report no block, not one outside itself")
	}
}

// A block promised to a workspace that is still being created has to count as
// taken. Without it, everything started during a creation — minutes, while Claude
// Code installs — picks the same "lowest free" block.
func TestTakenBlocksCountsReservations(t *testing.T) {
	held := []Holder{
		{Workspace: "crm", Host: "srv", Block: &PortBlock{Start: 16000, Size: 100}},
		{Workspace: "shop", Host: "srv", Block: nil}, // no block yet: nothing to take
	}
	reserved := []config.PortReservation{{Workspace: "new", Host: "srv", Start: 16100}}

	taken := takenBlocks(held, reserved)
	if !taken[16000] || !taken[16100] {
		t.Errorf("taken = %v, want both 16000 and 16100", taken)
	}
	if len(taken) != 2 {
		t.Errorf("taken = %v, want exactly two entries", taken)
	}

	r := config.PortRange{Start: 16000, End: 16299, Block: 100}
	got, ok := nextFreeBlock(r, taken)
	if !ok || got != 16200 {
		t.Errorf("next free = %d, %v; want 16200 — the reserved block must be skipped", got, ok)
	}
}

// Which workspaces have no block is the question `forge ports` is asked, so a
// list sorted by a value half of it lacks has to put those somewhere on purpose:
// last, where they read as the list of what still needs one.
func TestSortHoldersPutsTheBlocklessLast(t *testing.T) {
	held := []Holder{
		{Workspace: "none-a", Block: nil},
		{Workspace: "high", Block: &PortBlock{Start: 16200, Size: 100}},
		{Workspace: "none-b", Block: nil},
		{Workspace: "low", Block: &PortBlock{Start: 16000, Size: 100}},
	}
	sortHolders(held)

	if held[0].Workspace != "low" || held[1].Workspace != "high" {
		t.Errorf("blocks out of order: %+v", held)
	}
	for _, h := range held[2:] {
		if h.Block != nil {
			t.Errorf("a workspace with a block sorted after one without: %+v", held)
		}
	}
}

// The range and the block size are set by two different flags, either of which
// may be given alone. Whichever is not named has to survive: a `--block=` that
// silently reset the span to the default would move where every future block
// comes from, and blocks never move afterwards.
func TestSetPortRangeKeepsWhatWasNotAskedAbout(t *testing.T) {
	if _, err := SetPortRange(20000, 21000, 50); err != nil {
		t.Fatalf("SetPortRange: %v", err)
	}
	got, err := SetPortRange(0, 0, 25)
	if err != nil {
		t.Fatalf("block only: %v", err)
	}
	if got.Start != 20000 || got.End != 21000 || got.Block != 25 {
		t.Errorf("block only = %+v, want the span kept and the block 25", got)
	}
	got, err = SetPortRange(30000, 31000, 0)
	if err != nil {
		t.Fatalf("span only: %v", err)
	}
	if got.Start != 30000 || got.End != 31000 || got.Block != 25 {
		t.Errorf("span only = %+v, want the block size kept", got)
	}
	// And what it returns is what was written, not what was asked for.
	stored, err := PortRange()
	if err != nil {
		t.Fatal(err)
	}
	if stored != got {
		t.Errorf("stored %+v, reported %+v", stored, got)
	}

	// One bound alone leaves the other where it was. Moving them as a pair looks
	// harmless while the only caller parses a span and always has both — but a
	// ceiling raised on its own would take the floor to zero with it.
	got, err = SetPortRange(0, 32000, 0)
	if err != nil {
		t.Fatalf("end only: %v", err)
	}
	if got.Start != 30000 || got.End != 32000 || got.Block != 25 {
		t.Errorf("end only = %+v, want the start kept at 30000", got)
	}
	got, err = SetPortRange(31000, 0, 0)
	if err != nil {
		t.Fatalf("start only: %v", err)
	}
	if got.Start != 31000 || got.End != 32000 || got.Block != 25 {
		t.Errorf("start only = %+v, want the end kept at 32000", got)
	}

	// A span that holds no whole block is refused rather than stored, since every
	// allocation from it would fail.
	if _, err := SetPortRange(30000, 30010, 100); err == nil {
		t.Error("a range too small for one block should be refused")
	}
}

// A host that was told its range hands blocks out of it. That is the whole of
// what makes a second device safe to allocate from: which of a machine's ports
// are free is a fact about the machine, and a device that guessed would hand out
// one the machine had already given away.
func TestABlockComesFromTheHostsOwnRange(t *testing.T) {
	cfg := &config.Config{
		Hosts:      map[string]*config.Host{"a": {Addr: "10.0.0.1"}, "b": {Addr: "10.0.0.2"}},
		PortRange:  config.PortRange{Start: 16000, End: 30000, Block: 100},
		Workspaces: map[string]string{},
	}
	ranges := map[string]config.PortRange{
		"a": {Start: 20000, End: 21000, Block: 50},
	}

	if got := rangeFor(ranges, cfg, "a"); got.Start != 20000 || got.Block != 50 {
		t.Errorf("host a allocates from %+v, want its own 20000-21000/50", got)
	}
	// And a host nobody could ask keeps this client's range, which is what every
	// host used before any of them kept one — so nothing moves under a setup that
	// has not been migrated.
	if got := rangeFor(ranges, cfg, "b"); got.Start != 16000 || got.Block != 100 {
		t.Errorf("host b allocates from %+v, want the client's own", got)
	}
}

// A range answered as zeros is not a range, and using it would step the
// allocator by nothing. It falls back like a host that said nothing at all.
func TestAnEmptyAnswerFallsBackRatherThanAllocatingFromNothing(t *testing.T) {
	cfg := &config.Config{PortRange: config.PortRange{Start: 16000, End: 30000, Block: 100}}
	ranges := map[string]config.PortRange{"a": {}}

	got := rangeFor(ranges, cfg, "a")
	if got.Block == 0 {
		t.Fatal("the allocator was handed a range of nothing, which never terminates")
	}
	if got.Start != 16000 {
		t.Errorf("fell back to %+v, want the client's own range", got)
	}
}

// Only the machines being allocated on are asked.
//
// The fan-out is parallel, so the cost of asking one more is not a round trip —
// it is the *slowest* answer, and a host that is switched off does not answer at
// all until the connect timeout runs out. A server nobody is allocating on
// holding up the ones they are is the whole of what this avoids.
func TestOnlyTheHostsBeingAllocatedOnAreAsked(t *testing.T) {
	held := []Holder{
		{Workspace: "one", Host: "a"},
		{Workspace: "two", Host: "a"},
		{Workspace: "three", Host: "b"},
	}
	want := hostsOf(held)

	if len(want) != 2 || !want["a"] || !want["b"] {
		t.Errorf("would ask %v, want just the two hosts holding workspaces", want)
	}
	// Each machine once, however many workspaces are on it: "a" holds two.
	if want["idle"] {
		t.Error("a host holding nothing would be asked, and if it is off, waited for")
	}
}

// And a set nobody is in asks nobody, which is what an allocation with nothing
// to allocate should cost.
func TestAskingAboutNoHostsAsksNobody(t *testing.T) {
	cfg := &config.Config{
		Hosts:     map[string]*config.Host{"a": {Addr: "10.0.0.1"}},
		PortRange: config.PortRange{Start: 16000, End: 30000, Block: 100},
	}
	if got := hostRanges(cfg, map[string]bool{}); len(got) != 0 {
		t.Errorf("asked about %v when nothing needed a range", got)
	}
}
