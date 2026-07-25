package cli

import (
	"reflect"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
)

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

func TestParseSpan(t *testing.T) {
	start, end, err := parseSpan("16000-30000")
	if err != nil || start != 16000 || end != 30000 {
		t.Errorf("= %d, %d, %v", start, end, err)
	}
	if _, _, err := parseSpan(" 16000 - 30000 "); err != nil {
		t.Errorf("spaces should be tolerated: %v", err)
	}
	for _, bad := range []string{"", "16000", "abc-def", "30000-16000", "16000-70000", "80-9000", "16000-16000"} {
		if _, _, err := parseSpan(bad); err == nil {
			t.Errorf("parseSpan(%q) = nil error; want one", bad)
		}
	}
}

func TestParseLsofPorts(t *testing.T) {
	const out = `COMMAND   PID  USER   FD   TYPE DEVICE SIZE/OFF NODE NAME
node     4821 s1lent   23u  IPv4 0x1234      0t0  TCP *:16000 (LISTEN)
ssh      4900 s1lent    5u  IPv4 0x5678      0t0  TCP 127.0.0.1:16104 (LISTEN)
postgres  512 s1lent    7u  IPv6 0x9abc      0t0  TCP [::1]:5432 (LISTEN)
Dock      333 s1lent    9u  IPv4 0xdef0      0t0  TCP *:16000 (LISTEN)
`
	// Only what is inside the range, deduped and sorted — 5432 is outside it and
	// 16000 appears twice.
	if got, want := parseLsofPorts(out, 16000, 30000), []int{16000, 16104}; !reflect.DeepEqual(got, want) {
		t.Errorf("= %v, want %v", got, want)
	}
	if got := parseLsofPorts(out, 20000, 21000); len(got) != 0 {
		t.Errorf("nothing in range should be empty, got %v", got)
	}
	if got := parseLsofPorts("", 16000, 30000); len(got) != 0 {
		t.Errorf("no output = %v, want none", got)
	}
	// Header-only or truncated lines must not panic or produce ports.
	if got := parseLsofPorts("COMMAND PID USER\nnode 1\n", 1, 65535); len(got) != 0 {
		t.Errorf("short lines = %v, want none", got)
	}
}

// A block promised to a workspace that is still being created has to count as
// taken. Without it, everything started during a creation — minutes, while Claude
// Code installs — picks the same "lowest free" block.
func TestTakenBlocksCountsReservations(t *testing.T) {
	held := []holder{
		{workspace: "crm", alias: "srv", block: &agentproto.PortBlock{Start: 16000, Size: 100}},
		{workspace: "shop", alias: "srv", block: nil}, // no block yet: nothing to take
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
