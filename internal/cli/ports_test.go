package cli

import (
	"reflect"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/supervisor"
	"github.com/Marb-AI/forge/internal/ui"
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

func TestPortsInfoMergesObservationWithTunnelState(t *testing.T) {
	wp := agentproto.WorkspacePorts{
		Block: &agentproto.PortBlock{Start: 16000, Size: 100},
		Ports: []agentproto.Port{
			{Name: "web", Host: 16000, Target: 3000, Running: true, Kind: agentproto.KindContainer},
			{Name: "db", Host: 16001, Target: 5432, Running: false, Kind: agentproto.KindContainer},
			{Name: "vite", Host: 16002, Running: true, Kind: agentproto.KindProcess},
			// Outside the block: real, shown, never tunnelled.
			{Name: "stray", Host: 3000, Target: 3000, Running: true, Kind: agentproto.KindContainer},
		},
	}
	tunnels := map[int]supervisor.TunnelStatus{
		16000: {Port: 16000, State: supervisor.StateUp},
		16001: {Port: 16001, State: supervisor.StateBlocked, Detail: "held by node (pid 42)"},
		// 16002 has no row at all — the supervisor is not carrying it.
	}

	got := portsInfo(wp, tunnels)
	if got.Block != "16000-16099" {
		t.Errorf("block = %q", got.Block)
	}
	if len(got.Rows) != 4 {
		t.Fatalf("rows = %d", len(got.Rows))
	}
	by := map[int]ui.PortRow{}
	for _, r := range got.Rows {
		by[r.Port] = r
	}
	if r := by[16000]; r.Tunnel != supervisor.StateUp || !r.InBlock || r.Target != 3000 {
		t.Errorf("web = %+v", r)
	}
	if r := by[16001]; r.Tunnel != supervisor.StateBlocked || r.TunnelDetail == "" || r.Running {
		t.Errorf("db = %+v", r)
	}
	// No tunnel row is its own answer, not "up" and not a failure.
	if r := by[16002]; r.Tunnel != ui.TunnelNone || r.Kind != agentproto.KindProcess {
		t.Errorf("vite = %+v", r)
	}
	if r := by[3000]; r.InBlock {
		t.Errorf("a port outside the block was marked in-block: %+v", r)
	}
}

// A workspace with no block still reports its ports — it just cannot say any of
// them are carried, which is exactly what the panel needs to show.
func TestPortsInfoWithoutABlock(t *testing.T) {
	got := portsInfo(agentproto.WorkspacePorts{
		Ports: []agentproto.Port{{Name: "web", Host: 3000, Running: true, Kind: agentproto.KindContainer}},
	}, nil)
	if got.Block != "" {
		t.Errorf("block = %q, want empty", got.Block)
	}
	if len(got.Rows) != 1 || got.Rows[0].InBlock {
		t.Errorf("rows = %+v", got.Rows)
	}
}
