package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// The Claude session actions exposed to the UI: stop, restart, and the involved
// save-handoff-then-restart flow that is checkpoint. All three are the core's
// operations; this package implements none of them itself.
//
// Restart and checkpoint are the very same calls the CLI makes. Stop is not, yet:
// forge.StopSession also clears the session's clocks, where `forge workspace
// <name> claude stop` kills the tmux session and nothing else — and reports when
// there was no session to kill, which the core's stop deliberately tolerates.
// That drift predates the core and wants a PR that can decide what a stop should
// say, rather than one that moved code on the promise of changing no behaviour.
//
// What is left here is the HTTP shape: an unknown workspace is a 404, because the
// browser asked for something that does not exist, while a failure past that
// point is a 502 — the server we were told to reach did not answer.

// handleStop kills the workspace's Claude tmux session and ends its clocks with
// it: a stop is the end of a session, so the next one starts them over. The
// attached browser terminal sees the stream end; the session is gone from the
// server.
func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if err := s.deps.StopSession(ws); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("stop: %w", err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleRestart hard-restarts the session: kill it, then start a fresh detached
// Claude. The browser terminal reconnects and attaches to the new session.
func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if err := s.deps.RestartSession(ws); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("restart: %w", err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleTrackInc adds seconds of user-present time to a workspace's session
// tracking. The browser accumulates activity locally and flushes it here on a timer
// and when you leave, so the count survives a reload or a dropped connection. Best-
// effort: it never blocks the UI, and a workspace with no host just 404s.
func (s *server) handleTrackInc(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("bad request"))
		return
	}
	if req.Seconds <= 0 {
		writeJSON(w, map[string]bool{"ok": true}) // nothing to add
		return
	}
	if s.deps.TrackInc != nil {
		if err := s.deps.TrackInc(ws, req.Seconds); err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("track: %w", err))
			return
		}
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
	if !s.deps.KnowsWorkspace(ws) {
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
	if !s.deps.KnowsWorkspace(ws) {
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
