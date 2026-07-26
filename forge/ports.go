package forge

import (
	"fmt"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/supervisor"
)

// PortRow is one published port as the ports panel shows it: what is behind it,
// where to reach it, and whether that is true right now.
type PortRow struct {
	// Name is the compose service, or the process name for a plain listener.
	Name string `json:"name"`
	// Port is the host port, which is also the local port: a workspace's block is
	// unique across every server, so there is no mapping to show.
	Port int `json:"port"`
	// Target is the port inside the container. Shown nowhere, but it is what says
	// whether a browser link makes sense — a service on 5432 is Postgres however
	// it was published, and http://127.0.0.1:16003 would be a dead click.
	Target int `json:"target,omitempty"`
	// Running is the container's state. A stopped container keeps its row, because
	// its port is still spoken for and because starting it again is the thing you
	// came to the panel to do.
	Running bool `json:"running"`
	// Kind is "container" or "process". A plain process has no start/stop: there is
	// nothing to start it back up with that Forge could know.
	Kind string `json:"kind"`
	// Tunnel is the local tunnel's state — the supervisor's own vocabulary, plus
	// "none" for a port nothing is forwarding. It decides whether the link is live,
	// because a link to a port with no tunnel behind it is a lie.
	Tunnel string `json:"tunnel"`
	// TunnelDetail is why, when that is worth saying: which process is holding the
	// port locally, or what the connection failed with.
	TunnelDetail string `json:"tunnel_detail,omitempty"`
	// InBlock is false for a port published outside the workspace's own block.
	// Shown anyway — it is real and someone meant it — but it is never tunnelled,
	// so it needs to say why rather than look broken.
	InBlock bool `json:"in_block"`
}

// TunnelNone is the state of a port no tunnel is carrying: the supervisor has no
// row for it at all, which is different from having one that is failing.
const TunnelNone = "none"

// WorkspacePortsInfo is one workspace's block and rows.
type WorkspacePortsInfo struct {
	// Block is the range this workspace may publish in, "16000-16099", or empty if
	// it has none. Shown as the panel's subtitle, because "nothing here yet" and
	// "nothing here yet, and here is where it would go" are different messages.
	Block string    `json:"block,omitempty"`
	Rows  []PortRow `json:"rows"`
	// Note is why there are no rows, when the reason is not "nothing is running" —
	// today, a host that could not be reached. The same idea as HostStat's note: a
	// panel saying why it is empty beats one that just is, and beats a panel stuck
	// on "Loading…" for a server that is never going to answer.
	Note string `json:"note,omitempty"`
}

// Ports answers the browser's ports panel: what a workspace publishes, and whether
// each of those ports actually arrives on this machine.
//
// Two halves from two places, which is why it lives here rather than in either.
// The host knows what is published; only the local supervisor knows whether the
// tunnel carrying it is up, blocked by something on this laptop, or absent. A
// panel that showed one without the other would offer links that quietly fail.
func Ports(workspace string) (WorkspacePortsInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return WorkspacePortsInfo{}, err
	}
	host := cfg.HostFor(workspace)
	if host == nil {
		return WorkspacePortsInfo{}, fmt.Errorf("unknown workspace %q", workspace)
	}
	var res agentproto.PortsResult
	if err := CallAgent(host, &res, "workspace-ports"); err != nil {
		return WorkspacePortsInfo{}, err
	}
	return portsInfo(res.Workspaces[workspace], tunnelStates(workspace)), nil
}

// portsInfo turns one workspace's observation into rows, marking each with the
// state of the tunnel for its port.
func portsInfo(wp agentproto.WorkspacePorts, tunnels map[int]supervisor.TunnelStatus) WorkspacePortsInfo {
	info := WorkspacePortsInfo{Rows: []PortRow{}}
	if wp.Block != nil {
		info.Block = fmt.Sprintf("%d-%d", wp.Block.Start, wp.Block.End())
	}
	for _, p := range wp.Ports {
		row := PortRow{
			Name:    p.Name,
			Port:    p.Host,
			Target:  p.Target,
			Running: p.Running,
			Kind:    p.Kind,
			InBlock: wp.Block != nil && wp.Block.Contains(p.Host),
			Tunnel:  TunnelNone,
		}
		if t, ok := tunnels[p.Host]; ok {
			row.Tunnel, row.TunnelDetail = t.State, t.Detail
		}
		info.Rows = append(info.Rows, row)
	}
	return info
}

// tunnelStates reads the supervisor's status file for one workspace's tunnels. A
// supervisor that is not running, or has not written yet, yields nothing — which
// the panel shows as "no tunnel", because that is exactly what it means.
func tunnelStates(workspace string) map[int]supervisor.TunnelStatus {
	states := map[int]supervisor.TunnelStatus{}
	dir, err := config.Dir()
	if err != nil {
		return states
	}
	st, err := supervisor.ReadStatus(dir)
	if err != nil {
		return states
	}
	for _, t := range st.Tunnels {
		if t.Workspace == workspace {
			states[t.Port] = t
		}
	}
	return states
}
