package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Claude session actions exposed to the UI. stop and restart are simple tmux
// operations over ssh (implemented here); checkpoint is the involved
// save-handoff-then-restart flow, injected from the cli package via Deps so the
// UI and CLI share identical behaviour.

func (s *server) wsTarget(ws string) (sshx.Target, error) {
	h := s.deps.HostFor(ws)
	if h == nil {
		return sshx.Target{}, fmt.Errorf("unknown workspace %q", ws)
	}
	return sshx.WorkspaceTarget(h, ws), nil
}

// handleStop kills the workspace's Claude tmux session. The attached browser
// terminal sees the stream end; the session is gone from the server (like
// `forge workspace <name> claude stop`).
func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	target, err := s.wsTarget(r.PathValue("ws"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	// `|| true` makes "no such session" a success (stopping an already-stopped
	// session is a no-op) while leaving a genuine failure — an unreachable host —
	// as an error. Swallowing every error would report "stopped" for a server we
	// never even reached.
	if _, err := sshx.Capture(target.Args(agentproto.KillClaude)...); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("stop: %w", err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRestart hard-restarts the session: kill it, then start a fresh detached
// Claude. The browser terminal reconnects and attaches to the new session.
func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	target, err := s.wsTarget(r.PathValue("ws"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	// Kill then relaunch in one round trip; the kill tolerates "no session", so a
	// restart also works as a start.
	remote := agentproto.KillClaude + "; " + agentproto.StartClaude
	if _, err := sshx.Capture(target.Args(remote)...); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("restart: %w", err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleCheckpoint starts a checkpoint as a job (see jobs.go) and returns its
// id. It can take minutes — Claude writes a handoff, then the session restarts
// from memory — and it can fail outright (Claude busy), so the browser follows
// /api/jobs/{id}/stream for progress AND for the verdict. Firing and forgetting
// would leave a failed checkpoint looking like a running one forever.
//
// A second checkpoint for the same workspace while one is in flight is rejected.
func (s *server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if s.deps.HostFor(ws) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if !s.beginCheckpoint(ws) {
		writeJSONError(w, http.StatusConflict, fmt.Errorf("a checkpoint is already running"))
		return
	}
	id, err := s.startJob(func(out io.Writer) error {
		defer s.endCheckpoint(ws)
		return s.deps.Checkpoint(ws, out)
	})
	if err != nil {
		s.endCheckpoint(ws)
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"id": id})
}

// handleHosts lists the registered host aliases, so the wizard can offer them.
func (s *server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.deps.ListHosts()
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	if hosts == nil {
		hosts = []string{}
	}
	writeJSON(w, hosts)
}

// handleCreateWorkspace provisions a new workspace on a registered host. It
// blocks: creating the Linux user and its home on the server takes a moment, and
// the wizard wants a definite answer rather than a spinner that lies.
func (s *server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("bad request"))
		return
	}
	req.Name, req.Host = strings.TrimSpace(req.Name), strings.TrimSpace(req.Host)
	if !validName(req.Name) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("name must be 1-32 chars: letters, digits, dash or underscore"))
		return
	}
	if req.Host == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("pick a host"))
		return
	}
	if err := s.deps.CreateWorkspace(req.Name, req.Host); err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": req.Name, "host": req.Host})
}

// The administrative, mostly-irreversible operations, which is why they live
// behind the settings panel rather than a button you can hit by accident.

// handleDeleteWorkspace destroys a workspace on its host. This is the most
// destructive thing the UI can do: the agent runs `userdel -r`, so the workspace
// user and its entire home — all the code in it — are gone for good. The browser
// makes you type the name first; nothing can undo it.
func (s *server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if s.deps.HostFor(ws) == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	// Close our terminals for it first: an attached ssh session is a process of the
	// user being deleted, and `userdel` refuses to remove a user that still owns one.
	// The agent kills what remains (it has to — closing a connection here does not
	// make the far-side sshd exit instantly), but we still shut ours down rather
	// than making it race. The cost is that a delete which fails for some other
	// reason has still ended the session — the files are untouched and it
	// restarts, which is the cheaper of the two mistakes.
	s.terms.closeKeys(termKey(ws, termClaude), termKey(ws, termSSH))

	if err := s.deps.DeleteWorkspace(ws); err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRemoveHost forgets a server. The machine is untouched: its workspaces
// keep running, Forge just stops knowing about it — so this one is reversible,
// with `forge host add`.
func (s *server) handleRemoveHost(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if !s.knownHost(alias) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("no such host %q", alias))
		return
	}
	// Past that check a failure is ours (the config didn't save), not the user's —
	// reporting it as "not found" would send them looking for the wrong problem.
	if err := s.deps.RemoveHost(alias); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// knownHost reports whether alias is a registered server.
func (s *server) knownHost(alias string) bool {
	hosts, err := s.deps.ListHosts()
	if err != nil {
		return false
	}
	return slices.Contains(hosts, alias)
}

// handleSetUIPort records a new port for the UI. It cannot take effect now — this
// very daemon holds the old one — so the browser is told a restart is needed.
func (s *server) handleSetUIPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("bad request"))
		return
	}
	if err := s.deps.SetUIPort(req.Port); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "port": req.Port, "restart_required": true})
}

// validName keeps a workspace name or host alias safe as a Linux username and a
// tmux/compose identifier — the same shape the CLI accepts.
func validName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// beginCheckpoint marks ws as having a checkpoint in flight, returning false if
// one already is.
func (s *server) beginCheckpoint(ws string) bool {
	s.ckMu.Lock()
	defer s.ckMu.Unlock()
	if s.ckRunning[ws] {
		return false
	}
	s.ckRunning[ws] = true
	return true
}

func (s *server) endCheckpoint(ws string) {
	s.ckMu.Lock()
	delete(s.ckRunning, ws)
	s.ckMu.Unlock()
}
