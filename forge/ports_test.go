package forge

import (
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/supervisor"
)

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
	by := map[int]PortRow{}
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
	if r := by[16002]; r.Tunnel != TunnelNone || r.Kind != agentproto.KindProcess {
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
