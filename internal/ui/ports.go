package ui

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PortRow is one published port as the ports panel shows it: what is behind it,
// where to reach it, and whether that is true right now.
//
// The cli package fills these in — it is the one that can reach both halves of the
// answer, the host (what is published) and the local tunnel supervisor (whether it
// arrives). The ui package must not import either.
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
}

// handlePorts reports one workspace's published ports. Per workspace, not per
// host: the panel sits under the file tree, in the pane that is already about the
// workspace you are looking at, and asking for one costs one SSH round trip
// instead of one per registered server.
func (s *server) handlePorts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ws := r.PathValue("ws")
	if s.deps.Ports == nil {
		writeJSON(w, WorkspacePortsInfo{Rows: []PortRow{}})
		return
	}
	info, err := s.deps.Ports(ws)
	if err != nil {
		// Polled, so it degrades the way the other polled panels do: a host that
		// cannot be reached is a quiet empty panel, not a failed request that the
		// browser has to decide what to do with.
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	if info.Rows == nil {
		info.Rows = []PortRow{}
	}
	writeJSON(w, info)
}

// handleContainerAction starts or stops one of a workspace's containers.
//
// Only start and stop. Bringing a stack UP needs to know the project — which
// compose file, which profiles, whether it is really `make dev` — and Forge does
// not, so it does not offer it.
func (s *server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	if s.deps.ContainerAction == nil {
		writeJSONError(w, http.StatusNotImplemented, errNotWired)
		return
	}
	var req struct {
		Service string `json:"service"`
		Action  string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	if req.Service == "" {
		writeJSONError(w, http.StatusBadRequest, errNoService)
		return
	}
	if req.Action != "start" && req.Action != "stop" {
		writeJSONError(w, http.StatusBadRequest, errBadAction)
		return
	}
	if err := s.deps.ContainerAction(r.PathValue("ws"), req.Service, req.Action); err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type portsError string

func (e portsError) Error() string { return string(e) }

const (
	errNotWired  = portsError("container actions are not available")
	errNoService = portsError("service is required")
	errBadAction = portsError(`action must be "start" or "stop"`)
)
