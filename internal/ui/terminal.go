package ui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/creack/pty"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

// term is one live browser terminal: an ssh process behind a local pty, running
// either the Claude tmux session or a plain shell (see the kinds below). The
// local pty is what makes browser resizes real — resizing it raises SIGWINCH on
// ssh, which forwards the new size to the remote end.
//
// When the browser disconnects we kill the ssh process. For the Claude kind that
// is a tmux *detach*, so the session and Claude keep running server-side; for the
// ssh kind there is no tmux, so the shell goes with it.
type term struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func (t *term) close() {
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
}

// The two kinds of terminal the UI opens into a workspace.
const (
	// termClaude attaches the persistent Claude session (tmux): closing the
	// browser detaches, the session lives on.
	termClaude = "claude"
	// termSSH is a plain login shell as the workspace user — the panel you pop
	// open to run one command. It is NOT tmux-backed, so it lives exactly as long
	// as its stream: hiding the panel keeps the stream (and the shell) alive,
	// which is the whole point of hide-vs-close.
	termSSH = "ssh"
)

func validKind(k string) bool { return k == termClaude || k == termSSH }

// startTerm opens a terminal of the given kind into the workspace through a
// fresh local pty, sized to cols×rows so the very first draw matches the browser
// (a 0×0 or default pty makes tmux/Claude render into the wrong rectangle —
// cursor adrift, mouse tracking off).
func startTerm(h *config.Host, workspace, kind string, cols, rows uint16) (*term, error) {
	target := sshx.WorkspaceTarget(h, workspace)

	var args []string
	switch kind {
	case termClaude:
		args = target.TTYArgs(agentproto.AttachClaude)
	case termSSH:
		// Login shell with the local SSH agent forwarded — identical to
		// `forge workspace <name> ssh`, so git in the shell uses your keys.
		args = append([]string{"-A"}, target.TTYArgs()...)
	default:
		return nil, fmt.Errorf("unknown terminal kind %q", kind)
	}

	cmd := exec.Command("ssh", args...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	if cols > 0 && rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	}
	return &term{ptmx: ptmx, cmd: cmd}, nil
}

// termKey namespaces the registry by kind, so a workspace's Claude terminal and
// its ssh shell coexist instead of replacing each other.
func termKey(ws, kind string) string { return ws + "/" + kind }

// termRegistry holds at most one live terminal per key, where a key is a
// workspace *and* a kind (see termKey) — so a workspace's Claude session and its
// ssh shell coexist. Opening a new stream for a key replaces (and closes) any
// previous one, because a reconnect should supersede the attach it is replacing.
type termRegistry struct {
	mu sync.Mutex
	m  map[string]*term
}

func newTermRegistry() *termRegistry { return &termRegistry{m: map[string]*term{}} }

// replace installs t as the live terminal for key, closing whatever was there.
func (r *termRegistry) replace(key string, t *term) {
	r.mu.Lock()
	old := r.m[key]
	r.m[key] = t
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
}

// remove drops t if it is still the live terminal for key (a later stream may
// already have replaced it). It reports whether it removed exactly t — so a
// superseded handler can't close the terminal that replaced it.
func (r *termRegistry) remove(key string, t *term) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[key] == t {
		delete(r.m, key)
		return true
	}
	return false
}

func (r *termRegistry) get(key string) *term {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[key]
}

func (r *termRegistry) closeAll() {
	r.mu.Lock()
	terms := make([]*term, 0, len(r.m))
	for _, t := range r.m {
		terms = append(terms, t)
	}
	r.m = map[string]*term{}
	r.mu.Unlock()
	for _, t := range terms {
		t.close()
	}
}

// handleTermStream opens (or re-opens) the workspace's terminal and streams its
// output to the browser as Server-Sent Events. Each event's data is one
// base64-encoded chunk of raw pty output, so terminal escape codes and newlines
// survive SSE's line framing untouched.
func (s *server) handleTermStream(w http.ResponseWriter, r *http.Request) {
	ws, kind := r.PathValue("ws"), r.PathValue("kind")
	if !validKind(kind) {
		http.Error(w, "unknown terminal kind", http.StatusNotFound)
		return
	}
	h := s.deps.HostFor(ws)
	if h == nil {
		http.Error(w, "unknown workspace", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// The browser passes its measured terminal size so the pty is correct from
	// the first byte. Fall back to a sane default if absent/garbage.
	cols := parseDim(r.URL.Query().Get("cols"), 80)
	rows := parseDim(r.URL.Query().Get("rows"), 24)

	key := termKey(ws, kind)
	t, err := startTerm(h, ws, kind, cols, rows)
	if err != nil {
		http.Error(w, "start terminal: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.terms.replace(key, t)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	defer func() {
		if s.terms.remove(key, t) {
			t.close()
		}
	}()

	// Read the pty in a goroutine so we can also watch for client disconnect.
	// Every send is guarded by ctx: once the browser is gone nobody drains this
	// channel, and a bare send would block the goroutine forever, leaking it (and
	// the pty it holds) for the life of the daemon.
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, 16)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := t.ptmx.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				select {
				case chunks <- chunk{data: b}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				select {
				case chunks <- chunk{err: err}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-chunks:
			if len(c.data) > 0 {
				enc := base64.StdEncoding.EncodeToString(c.data)
				if _, err := fmt.Fprintf(w, "data: %s\n\n", enc); err != nil {
					return
				}
				flusher.Flush()
			}
			if c.err != nil {
				// ssh/tmux ended (session stopped, disconnect). Tell the browser.
				_, _ = io.WriteString(w, "event: end\ndata: \n\n")
				flusher.Flush()
				return
			}
		}
	}
}

// handleTermInput writes keystrokes from the browser to the workspace terminal.
// The body is base64 (xterm sends arbitrary bytes, including control sequences).
func (s *server) handleTermInput(w http.ResponseWriter, r *http.Request) {
	t := s.terms.get(termKey(r.PathValue("ws"), r.PathValue("kind")))
	if t == nil {
		http.Error(w, "no terminal", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	data, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		http.Error(w, "bad encoding", http.StatusBadRequest)
		return
	}
	if _, err := t.ptmx.Write(data); err != nil {
		http.Error(w, "write", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTermResize applies a browser resize to the pty, which propagates the new
// window size through ssh to the remote tmux client.
func (s *server) handleTermResize(w http.ResponseWriter, r *http.Request) {
	t := s.terms.get(termKey(r.PathValue("ws"), r.PathValue("kind")))
	if t == nil {
		http.Error(w, "no terminal", http.StatusNotFound)
		return
	}
	var size struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&size); err != nil {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	if size.Cols == 0 || size.Rows == 0 {
		http.Error(w, "zero size", http.StatusBadRequest)
		return
	}
	_ = pty.Setsize(t.ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
	w.WriteHeader(http.StatusNoContent)
}

// parseDim parses a terminal dimension query param, clamping to a sane range so
// a bogus value can't create an absurd pty. Falls back to def when empty/invalid.
func parseDim(s string, def uint16) uint16 {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return def
	}
	return uint16(n)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
