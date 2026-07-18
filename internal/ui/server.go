// Package ui serves Forge's local browser UI: tabs per workspace, a live
// terminal into each workspace's Claude session, checkpoint/restart/stop, a
// read-only file browser, and an ssh shell that overlays the terminal. It runs
// as a detached daemon started by `forge ui`, binds to 127.0.0.1 only, and
// reuses the same ssh/tmux plumbing the CLI uses — so the UI is a second front
// end over the exact same actions, not a reimplementation of them.
//
// Security model (localhost, no login): the server binds to the loopback
// interface, validates the Host header (so a rebound DNS name can't reach it),
// gates every request on a random per-session token delivered once via the URL
// and then held in a Strict-SameSite cookie, and rejects cross-origin
// state-changing requests. That keeps another local user — or any web page open
// in the same browser — from driving your workspaces, without a password.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Marb-AI/forge/internal/config"
)

// WorkspaceInfo is one tab in the UI: a workspace, the host it lives on, and its
// Claude session status. It mirrors the CLI's workspace list; the cli package
// fills it in (the ui package must not import cli).
type WorkspaceInfo struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Status string `json:"status"`
}

// Activity is a workspace's Claude attention state (state is "busy"/"idle"/
// "waiting"), with the unix second the state was set — the cli package fills it
// in from the agent (the ui package must not import agentproto).
type Activity struct {
	State string `json:"state"`
	TS    int64  `json:"ts"`
}

// Deps are the Forge operations the UI needs, injected by the cli package so the
// ui package stays free of the agent/command machinery (and of import cycles).
type Deps struct {
	// ListWorkspaces returns the current workspaces across all hosts.
	ListWorkspaces func() ([]WorkspaceInfo, error)
	// WorkspaceActivity returns each workspace's Claude attention state, keyed by
	// name. Polled by the UI to light up tabs where Claude is waiting for you.
	WorkspaceActivity func() (map[string]Activity, error)
	// HostFor resolves a workspace name to the host it lives on, or nil.
	HostFor func(name string) *config.Host
	// Checkpoint saves a handoff to memory and restarts the session from it. It
	// blocks for minutes and can fail (Claude busy), so it runs as a job and
	// reports progress to out. Injected by the cli package (the exact CLI logic).
	Checkpoint func(name string, out io.Writer) error
	// ListHosts returns the registered host aliases (for the new-workspace wizard).
	ListHosts func() ([]string, error)
	// CreateWorkspace provisions a workspace on a registered host. It talks to the
	// server, so it can take a while.
	CreateWorkspace func(name, host string) error
	// PrepareHost provisions a bare server and registers it. It takes minutes and
	// its progress is the point, so it writes every line to out (an SSE stream).
	PrepareHost func(sshTarget, alias string, firewall, harden, dockerPrune bool, out io.Writer) error
	// DeleteWorkspace destroys a workspace on its host. IRREVERSIBLE: the agent
	// runs `userdel -r`, so the workspace user and every file in its home are gone.
	DeleteWorkspace func(name string) error
	// RemoveHost forgets a server locally. The machine is untouched — its
	// workspaces keep running, Forge just stops knowing about them.
	RemoveHost func(alias string) error
	// SetUIPort records the port the UI binds to. Takes effect on the next start.
	SetUIPort func(port int) error
}

// validate reports the first operation the caller forgot to wire. Every field is
// required: the UI offers all of them, so a missing one is a bug in the wiring,
// not a feature the user opted out of.
func (d Deps) validate() error {
	for name, fn := range map[string]any{
		"ListWorkspaces":  d.ListWorkspaces,
		"HostFor":         d.HostFor,
		"Checkpoint":      d.Checkpoint,
		"ListHosts":       d.ListHosts,
		"CreateWorkspace": d.CreateWorkspace,
		"PrepareHost":     d.PrepareHost,
		"DeleteWorkspace": d.DeleteWorkspace,
		"RemoveHost":      d.RemoveHost,
		"SetUIPort":       d.SetUIPort,
	} {
		if reflect.ValueOf(fn).IsNil() {
			return fmt.Errorf("ui: Deps.%s is not wired", name)
		}
	}
	return nil
}

// cookieName holds the session token in the browser after the one-time
// token-in-URL bootstrap.
const cookieName = "forge_ui"

type server struct {
	token string
	deps  Deps
	terms *termRegistry

	ckMu      sync.Mutex      // guards ckRunning
	ckRunning map[string]bool // workspaces with a checkpoint in flight

	jobMu sync.Mutex      // guards jobs
	jobs  map[string]*job // long-running operations, followed over SSE
}

// PIDPath returns the ui daemon's pidfile location (sibling to the supervisor's).
func PIDPath(dir string) string { return filepath.Join(dir, "ui.pid") }

// TokenPath returns the session token's location. The daemon writes it; `forge ui`
// reads it back to build the URL it opens.
func TokenPath(dir string) string { return filepath.Join(dir, "ui.token") }

// newToken mints a session token.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Serve runs the UI daemon: it binds to 127.0.0.1:port, claims the pidfile once
// the bind succeeded, and blocks serving requests until the process is signalled
// (SIGINT/SIGTERM). This is the body of the detached `forge ui` daemon.
//
// The order matters: `forge ui` waits for the pidfile to decide the daemon is
// up, so the pidfile must mean "bound and serving", never "started and about to
// fail on a port that's already taken".
func Serve(dir string, port int, deps Deps) error {
	// Fail fast on an incomplete wiring rather than nil-checking in a dozen
	// handlers (and panicking in the ones that forget).
	if err := deps.validate(); err != nil {
		return err
	}

	// Bind BEFORE anything else: loopback only, so nothing off this machine can
	// reach the UI. If the port is taken we fail here, and `forge ui` — which
	// waits for the pidfile — reports that instead of cheerfully opening a
	// browser at a dead address.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", port, err)
	}

	// The token is minted HERE, by the daemon that won the port — not by the
	// command that spawned it. Two `forge ui` racing each other would otherwise
	// each write a token, and the URL one of them printed would open a session
	// the surviving daemon has never heard of. The winner writes the token, then
	// the pidfile; `forge ui` waits for the pidfile, so by the time it reads the
	// token, the token it reads is the one being served.
	token, err := newToken()
	if err != nil {
		_ = ln.Close()
		return err
	}
	if err := os.WriteFile(TokenPath(dir), []byte(token), 0o600); err != nil {
		_ = ln.Close()
		return err
	}

	s := &server{
		token: token, deps: deps,
		terms:     newTermRegistry(),
		ckRunning: map[string]bool{},
		jobs:      map[string]*job{},
	}

	if err := os.WriteFile(PIDPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	defer os.Remove(PIDPath(dir))

	srv := &http.Server{
		Handler: s.handler(),
		// Bound the header read so a stuck connection can't hold a slot forever.
		// Deliberately no WriteTimeout: the terminal and job streams are SSE and
		// stay open for as long as you're watching them.
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		s.terms.closeAll()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// assetFS returns the filesystem the UI is served from. Normally that is the
// embedded assets (single binary); when FORGE_UI_DEV points at the repo root,
// assets are served live from disk so UI edits show up on reload with no
// rebuild. The returned FS is rooted at the assets directory either way.
func assetFS() fs.FS {
	if dev := os.Getenv("FORGE_UI_DEV"); dev != "" {
		return os.DirFS(filepath.Join(dev, "internal", "ui", "assets"))
	}
	sub, _ := fs.Sub(assetsFS, "assets")
	return sub
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	assets := http.StripPrefix("/assets/", http.FileServer(http.FS(assetFS())))
	mux.Handle("GET /assets/", noCache(assets))
	mux.HandleFunc("GET /api/workspaces", s.handleWorkspaces)
	mux.HandleFunc("GET /api/activity", s.handleActivity)
	mux.HandleFunc("GET /api/term/{ws}/{kind}/stream", s.handleTermStream)
	mux.HandleFunc("POST /api/term/{ws}/{kind}/input", s.handleTermInput)
	mux.HandleFunc("POST /api/term/{ws}/{kind}/resize", s.handleTermResize)
	mux.HandleFunc("POST /api/ws/{ws}/stop", s.handleStop)
	mux.HandleFunc("POST /api/ws/{ws}/restart", s.handleRestart)
	mux.HandleFunc("POST /api/ws/{ws}/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("GET /api/fs/{ws}/list", s.handleFsList)
	mux.HandleFunc("GET /api/fs/{ws}/read", s.handleFsRead)
	mux.HandleFunc("GET /api/hosts", s.handleHosts)
	mux.HandleFunc("POST /api/workspaces", s.handleCreateWorkspace)
	mux.HandleFunc("POST /api/hosts/prepare", s.handlePrepareHost)
	mux.HandleFunc("GET /api/jobs/{id}/stream", s.handleJobStream)
	mux.HandleFunc("DELETE /api/workspaces/{ws}", s.handleDeleteWorkspace)
	mux.HandleFunc("DELETE /api/hosts/{alias}", s.handleRemoveHost)
	mux.HandleFunc("PUT /api/config/ui-port", s.handleSetUIPort)
	return s.guard(mux)
}

// guard enforces the security model on every request: loopback Host, a valid
// session token (bootstrapped from the URL into a Strict-SameSite cookie), and a
// same-origin check on state-changing requests.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// One-time bootstrap: a correct ?t=<token> in the URL promotes to a
		// cookie, then we redirect to strip the token from the address bar.
		if t := r.URL.Query().Get("t"); t != "" {
			if tokenEqual(t, s.token) {
				http.SetCookie(w, &http.Cookie{
					Name: cookieName, Value: s.token, Path: "/",
					HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				u := *r.URL
				q := u.Query()
				q.Del("t")
				u.RawQuery = q.Encode()
				http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if c, err := r.Cookie(cookieName); err != nil || !tokenEqual(c.Value, s.token) {
			http.Error(w, "forbidden — open the URL that `forge ui` printed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "bad origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(assetFS(), "index.html")
	if err != nil {
		http.Error(w, "ui asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// noCache stops the browser caching UI assets — the daemon is restarted often
// during development, and a stale app.js is a confusing way to lose a fix.
// no-store (not no-cache) so an already-cached copy can't be served either.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.ListWorkspaces()
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	if list == nil {
		list = []WorkspaceInfo{}
	}
	writeJSON(w, list)
}

// handleActivity returns each workspace's Claude attention state, keyed by name.
// The UI polls this on a short interval; a host we can't reach just contributes
// nothing, so a slow or down host dims its tabs rather than failing the request.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	act := map[string]Activity{}
	if s.deps.WorkspaceActivity != nil {
		if a, err := s.deps.WorkspaceActivity(); err == nil && a != nil {
			act = a
		}
	}
	writeJSON(w, act)
}

// loopbackHost reports whether the request's Host header names the loopback
// interface — the DNS-rebinding defense. r.Host may carry a port.
func loopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	switch h {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	// Any address that resolves to loopback (e.g. 127.0.0.2) is also fine.
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// sameOrigin reports whether a state-changing request came from this very UI —
// the exact scheme, host AND port we are serving on.
//
// "It's loopback" is not enough, and that was a real hole. SameSite is defined
// over *sites*, and a site ignores the port: to the browser, a page on
// http://127.0.0.1:9999 is the same site as this UI, so it gets our
// Strict-SameSite cookie attached automatically. CORS doesn't save us either —
// a POST with Content-Type: text/plain is a "simple" request and is sent without
// a preflight, which is exactly the shape of our own /input endpoint. So any web
// app you happen to be running on any other localhost port could type into your
// Claude session, or stop it, just by asking.
//
// Requiring the Origin to match our own origin exactly closes that: a page on
// another port cannot forge one, because the browser sets it.
//
// An absent Origin is allowed: browsers attach Origin to every cross-origin
// request and to same-origin writes, so no Origin means no browser-driven
// cross-site request. A local tool holding your own token is you.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.Scheme != "http" {
		return false // malformed, another scheme, or the literal "null"
	}
	return u.Host == r.Host
}

func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
