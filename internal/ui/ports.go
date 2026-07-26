package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Marb-AI/forge/forge"
)

// The ports panel's types, like the rest, are the core's own — see the type block
// in server.go for why.
type (
	// PortRow is one published port as the panel shows it: what is behind it,
	// where to reach it, and whether that is true right now.
	PortRow = forge.PortRow
	// WorkspacePortsInfo is one workspace's block and rows.
	WorkspacePortsInfo = forge.WorkspacePortsInfo
)

// TunnelNone is the state of a port no tunnel is carrying: the supervisor has no
// row for it at all, which is different from having one that is failing.
const TunnelNone = forge.TunnelNone

// handlePorts reports one workspace's published ports. Per workspace, not per
// host: the panel sits under the file tree, in the pane that is already about the
// workspace you are looking at, and asking for one costs one SSH round trip
// instead of one per registered server.
func (s *server) handlePorts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ws := r.PathValue("ws")
	// A workspace this client does not have is a 404, like every other per-workspace
	// endpoint. It is not a transient failure and no amount of polling will fix it,
	// so it must not be dressed up as one.
	if s.deps.HostFor(ws) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if s.deps.Ports == nil {
		writeJSON(w, WorkspacePortsInfo{Rows: []PortRow{}})
		return
	}
	info, err := s.deps.Ports(ws)
	if err != nil {
		// Everything left is the host not answering, which is transient and which
		// the other polled panels report rather than fail on. A 502 here would leave
		// the panel on "Loading…" for as long as the server stays down, because the
		// browser keeps its last good answer and there was never one.
		writeJSON(w, WorkspacePortsInfo{Rows: []PortRow{}, Note: unreachableNote(err)})
		return
	}
	if info.Rows == nil {
		info.Rows = []PortRow{}
	}
	writeJSON(w, info)
}

// unreachableNote is the short line the empty panel shows. The error itself is an
// ssh failure several layers down and reads like one; what the panel has room for
// is the fact.
func unreachableNote(err error) string {
	const short = "Can't reach the server."
	if err == nil {
		return short
	}
	return short + " " + firstLine(err.Error())
}

// firstLine keeps a multi-line ssh error from turning the panel into a paragraph.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// handleContainerAction starts or stops one of a workspace's containers.
//
// Only start and stop. Bringing a stack UP needs to know the project — which
// compose file, which profiles, whether it is really `make dev` — and Forge does
// not, so it does not offer it.
func (s *server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if s.deps.HostFor(ws) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
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
	if err := s.deps.ContainerAction(ws, req.Service, req.Action); err != nil {
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
