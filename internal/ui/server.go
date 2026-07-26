// Package ui serves Forge's local browser UI: tabs per workspace, a live
// terminal into each workspace's Claude session, checkpoint/restart/stop, a
// read-only file browser, and shells that overlay the terminal — on the
// workspace, on the host, and on this machine. It binds to 127.0.0.1 only, and
// runs every operation through package forge, the same core the CLI runs — so the
// UI is a second front end over the exact same actions, not a reimplementation of
// them.
//
// It comes up two ways over the same server: as the detached daemon `forge ui`
// spawns (Serve), and in the caller's own process (Start, inprocess.go) for a
// desktop shell that has a window instead of a pidfile.
//
// Security model (localhost, no login): the server binds to the loopback
// interface, validates the Host header (so a rebound DNS name can't reach it),
// gates every request on a random per-session token delivered once via the URL
// and then held in a Strict-SameSite cookie, and rejects cross-origin
// state-changing requests. That keeps another local user — or any web page open
// in the same browser — from driving your workspaces, without a password. Note
// what the token is worth: it has always been able to run commands as you on your
// servers, and since the local shell it can do the same on this machine. The
// checks above are what stand between it and anything else on your loopback.
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

	"github.com/Marb-AI/forge/forge"
)

// The types the UI serves are the core's own: this package is a front end over
// forge, not a translation of it, so a field the browser reads is the field the
// operation returned — no copying, and no wire format that can drift from what the
// core says. Each alias is named here rather than referred to as forge.X so the
// handlers below (and their tests) read as they always did.
type (
	// WorkspaceInfo is one tab in the UI: a workspace, the host it lives on, and
	// its Claude session status.
	WorkspaceInfo = forge.WorkspaceInfo
	// Activity is what a workspace's Claude is up to — its attention state and the
	// topic it last wrote — polled to light up tabs where Claude is waiting.
	Activity = forge.Activity
	// Track is a workspace's session tracking, the two clocks in the banner.
	Track = forge.Track
	// Usage is one workspace's Claude usage: login, context, cost, rate limits.
	Usage = forge.Usage
	// Account identifies the Claude login a workspace runs as; the usage panel
	// groups by it.
	Account = forge.Account
	// RateWindow is one of a subscription's rate-limit windows. Nil means absent,
	// which is not 0%.
	RateWindow = forge.RateWindow
	// HostStat is one registered server's resource usage, for the servers panel.
	HostStat = forge.HostStat
	// Terminal is one live terminal — the Claude session, a shell, the local shell
	// — handed over by the core with the process behind it already running.
	Terminal = forge.Terminal
)

// Deps are the Forge operations the UI needs, injected rather than called
// directly so this package stays free of the agent/command machinery (and of the
// SSH round trips, which is what lets the handlers be tested at all).
//
// Every one of them is now the core's own function, wired a line each. Nothing
// here is implemented by the front end that wires it: this struct is the list of
// what the UI asks Forge to do, and the tests fill it with fakes.
type Deps struct {
	// ListWorkspaces returns the current workspaces across all hosts.
	ListWorkspaces func() ([]WorkspaceInfo, error)
	// WorkspaceActivity returns each workspace's Claude attention state, keyed by
	// name. Polled by the UI to light up tabs where Claude is waiting for you.
	// Optional — deliberately NOT in validate(): handleActivity nil-checks it and
	// falls back to an empty map, so a caller that doesn't wire it just reports no
	// activity rather than failing to start.
	WorkspaceActivity func() (map[string]Activity, error)
	// WorkspaceTrack returns each workspace's session tracking (when the session
	// began, how long the user has been present), keyed by name. Polled by the UI to
	// drive the tracking banner's two clocks. Optional, like WorkspaceActivity:
	// handleTrack nil-checks it and reports no tracking rather than failing.
	WorkspaceTrack func() (map[string]Track, error)
	// WorkspaceUsage returns each workspace's Claude usage — login, context, cost and
	// that login's rate-limit windows — keyed by name. Polled by the UI to group the
	// workspaces by login and show which logins are near their limit. Optional, like
	// WorkspaceActivity: handleUsage nil-checks it and reports none rather than
	// failing to start.
	WorkspaceUsage func() (map[string]Usage, error)
	// TrackInc adds seconds of user-present time to a workspace's tracking. The UI
	// flushes its accumulated activity here periodically. Optional: handleTrackInc
	// nil-checks it, so a caller that doesn't wire it just doesn't persist activity.
	TrackInc func(name string, seconds int) error
	// HostStats returns every registered server's resource usage, one entry per
	// host, ordered by alias. Polled by the UI's servers panel. Optional, like
	// WorkspaceActivity: handleHostStats nil-checks it and reports an empty list
	// rather than failing to start.
	HostStats func() ([]HostStat, error)
	// KnowsWorkspace reports whether this client has a workspace by that name —
	// what every per-workspace endpoint asks before doing anything, so an unknown
	// name is a 404 rather than an attempt that fails.
	//
	// It used to hand back the host itself, because the terminals and the file
	// browser built their own ssh commands from it. They ask the core now, so what
	// is left of that question is the answer yes or no: which machine a workspace
	// is on, and what it takes to log in, is not this package's business.
	KnowsWorkspace func(name string) bool
	// OpenTerminal opens a terminal — the Claude session, a shell on the workspace
	// or its host, or the local shell — sized to cols×rows. The kinds are the
	// core's (termClaude, termSSH, termHost, forge.TermLocal); the local one takes
	// no workspace.
	OpenTerminal func(kind, workspace string, cols, rows uint16) (Terminal, error)
	// ListDir lists a directory in the workspace, relative to its home. Empty is
	// the home itself.
	ListDir func(workspace, dir string) (DirListing, error)
	// ReadFile returns as much of a file's text as the viewer gets.
	ReadFile func(workspace, file string) (FileText, error)
	// Checkpoint saves a handoff to memory and restarts the session from it. It
	// blocks for minutes and can fail (Claude busy), so it runs as a job and
	// reports progress to out.
	Checkpoint func(name string, out io.Writer) error
	// StopSession kills the workspace's Claude session, ending its clocks with it.
	StopSession func(name string) error
	// RestartSession kills the session and starts a fresh one. A hard restart is a
	// new session, so its clocks start over — unlike a checkpoint's.
	RestartSession func(name string) error
	// ListHosts returns the registered host aliases (for the new-workspace wizard).
	ListHosts func() ([]string, error)
	// CreateWorkspace provisions a workspace on a registered host. It talks to the
	// server, so it can take a while.
	CreateWorkspace func(name, host string) error
	// PrepareHost provisions a bare server and registers it. It takes minutes and
	// its progress is the point, so it writes every line to out (an SSE stream).
	PrepareHost func(sshTarget, alias string, firewall, harden, dockerPrune, pruneImages bool, out io.Writer) error
	// DeleteWorkspace destroys a workspace on its host. IRREVERSIBLE: the agent
	// runs `userdel -r`, so the workspace user and every file in its home are gone.
	DeleteWorkspace func(name string) error
	// RemoveHost forgets a server locally. The machine is untouched — its
	// workspaces keep running, Forge just stops knowing about them.
	RemoveHost func(alias string) error
	// SetUIPort records the port the UI binds to. Takes effect on the next start.
	SetUIPort func(port int) error
	// Ports reports one workspace's published ports and the state of the tunnel
	// carrying each. Injected because it needs both halves — the host, and the
	// local tunnel supervisor — and this package must import neither.
	Ports func(workspace string) (WorkspacePortsInfo, error)
	// ContainerAction starts or stops one of a workspace's containers. Never
	// creates one: that would need to know the project.
	ContainerAction func(workspace, service, action string) error
}

// validate reports the first operation the caller forgot to wire. Every field is
// required: the UI offers all of them, so a missing one is a bug in the wiring,
// not a feature the user opted out of.
func (d Deps) validate() error {
	for name, fn := range map[string]any{
		"ListWorkspaces":  d.ListWorkspaces,
		"KnowsWorkspace":  d.KnowsWorkspace,
		"OpenTerminal":    d.OpenTerminal,
		"ListDir":         d.ListDir,
		"ReadFile":        d.ReadFile,
		"Checkpoint":      d.Checkpoint,
		"StopSession":     d.StopSession,
		"RestartSession":  d.RestartSession,
		"ListHosts":       d.ListHosts,
		"CreateWorkspace": d.CreateWorkspace,
		"PrepareHost":     d.PrepareHost,
		"DeleteWorkspace": d.DeleteWorkspace,
		"RemoveHost":      d.RemoveHost,
		"SetUIPort":       d.SetUIPort,
		"Ports":           d.Ports,
		"ContainerAction": d.ContainerAction,
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

	wsMu       sync.Mutex       // guards the fields below
	wsInFlight *wsListCall      // the ListWorkspaces call callers are currently sharing
	wsLast     *wsListCall      // the last call that completed, for maxAge to reuse
	wsLastAt   time.Time        // when wsLast finished
	now        func() time.Time // overridable in tests
	onJoin     func()           // test seam: a caller just joined an in-flight call

	statsMu       sync.Mutex // guards the fields below
	statsInFlight *statsCall // the HostStats call callers are currently sharing
	statsLast     *statsCall // the last one that completed, reused within statsFresh
	statsLastAt   time.Time  // when statsLast finished
	onStatsJoin   func()     // test seam, as onJoin
}

// statsCall is one in-flight HostStats, and the result everyone waiting on it
// will get.
type statsCall struct {
	done  chan struct{}
	stats []HostStat
	err   error
}

// wsListCall is one in-flight ListWorkspaces, and the result everyone waiting on
// it will get. See handleWorkspaces for why they share.
type wsListCall struct {
	done chan struct{}
	list []WorkspaceInfo
	err  error
}

// Run is the body of the detached `forge ui` daemon: it resolves where the
// config lives and which port to bind, wires every operation to the core, and
// serves until the process is signalled.
//
// It lives here rather than in whichever command spawned the daemon, because the
// wiring below is this front end's own statement of what it needs from Forge.
func Run() error {
	dir, err := forge.StateDir()
	if err != nil {
		return err
	}
	port, err := forge.UIPort()
	if err != nil {
		return err
	}
	return Serve(dir, port, CoreDeps())
}

// CoreDeps wires the UI to the real Forge core: one line per operation, none of
// them implemented here. It is the seam the whole package exists on the far side
// of — tests build a Deps of fakes instead, and the handlers cannot tell.
func CoreDeps() Deps {
	return Deps{
		ListWorkspaces:    forge.ListWorkspaces,
		WorkspaceActivity: forge.WorkspaceActivity,
		WorkspaceTrack:    forge.WorkspaceTrack,
		WorkspaceUsage:    forge.WorkspaceUsage,
		TrackInc:          forge.TrackInc,
		HostStats:         forge.HostStats,
		KnowsWorkspace:    forge.KnowsWorkspace,
		OpenTerminal:      forge.OpenTerminal,
		ListDir:           forge.ListDir,
		ReadFile:          forge.ReadFile,
		Checkpoint:        forge.Checkpoint,
		StopSession:       forge.StopSession,
		RestartSession:    forge.RestartSession,
		ListHosts:         forge.ListHosts,
		// The block the workspace was given is dropped here: the browser wizard has
		// nowhere to say it yet. Nothing is lost — it is on the workspace, and
		// `forge ports` reports it.
		CreateWorkspace: func(name, host string) error {
			_, err := forge.CreateWorkspace(name, host)
			return err
		},
		PrepareHost:     forge.PrepareHost,
		DeleteWorkspace: forge.DeleteWorkspace,
		RemoveHost:      forge.RemoveHost,
		SetUIPort:       forge.SetUIPort,
		Ports:           forge.Ports,
		ContainerAction: forge.ContainerAction,
	}
}

// URL is the address the UI on that port is reached at, carrying the token that
// gets past the guard on first request. An empty token yields the bare address,
// which is what to print when nothing is known to be serving — it will ask for
// one.
//
// It lives here, in the package that mints the token and checks it, because both
// front ends need to say the same thing: the CLI prints it for the daemon it
// spawned, and a desktop shell points its webview at the instance it started.
func URL(port int, token string) string {
	if token == "" {
		return fmt.Sprintf("http://127.0.0.1:%d/", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)
}

// newToken mints a session token.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// bind is everything Serve and Start do identically: check the wiring, take the
// port, and mint the token this run will be reached with. It stops exactly where
// the two part company — at what owns the process.
//
// Binding comes BEFORE anything else, loopback only, so nothing off this machine
// can reach the UI. A port that is already taken fails here, which is what lets
// `forge ui` report it instead of cheerfully opening a browser at a dead address.
//
// The token is minted HERE, by whoever won the port — not by the command that
// spawned it. Two `forge ui` racing each other would otherwise each write a token,
// and the URL one of them printed would open a session the surviving daemon has
// never heard of. Port 0 asks the OS for a free one; the caller reads back which,
// off the listener.
func bind(port int, deps Deps) (net.Listener, *server, error) {
	// Fail fast on an incomplete wiring rather than nil-checking in a dozen
	// handlers (and panicking in the ones that forget).
	if err := deps.validate(); err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", port, err)
	}
	token, err := newToken()
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	return ln, &server{
		token: token, deps: deps,
		terms:     newTermRegistry(),
		ckRunning: map[string]bool{},
		jobs:      map[string]*job{},
		now:       time.Now,
	}, nil
}

// httpServer is the http.Server both entry points serve with, so a timeout set
// for the daemon is the same timeout in a desktop shell.
func httpServer(s *server) *http.Server {
	return &http.Server{
		Handler: s.handler(),
		// Bound the header read so a stuck connection can't hold a slot forever.
		// Deliberately no WriteTimeout: the terminal and job streams are SSE and
		// stay open for as long as you're watching them.
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// shutdown closes the live terminals and stops serving. The terminals go first
// and by hand: they are the processes this UI opened, and an http.Server's own
// shutdown would wait on their streams rather than end them.
func shutdown(srv *http.Server, s *server) error {
	s.terms.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Serve runs the UI daemon: it binds to 127.0.0.1:port, claims the pidfile once
// the bind succeeded, and blocks serving requests until the process is signalled
// (SIGINT/SIGTERM). This is the body of the detached `forge ui` daemon.
//
// The order matters: `forge ui` waits for the pidfile to decide the daemon is
// up, so the pidfile must mean "bound and serving", never "started and about to
// fail on a port that's already taken". The winner writes the token, then the
// pidfile; `forge ui` waits for the pidfile, so by the time it reads the token,
// the token it reads is the one being served.
//
// The pidfile, the token file and the signal handler are this function's whole
// subject: they are what makes the UI a daemon on a laptop, and they are what
// Start does without. See inprocess.go.
func Serve(dir string, port int, deps Deps) error {
	ln, s, err := bind(port, deps)
	if err != nil {
		return err
	}
	if err := os.WriteFile(forge.UITokenPath(dir), []byte(s.token), 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	if err := os.WriteFile(forge.UIPIDPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	defer os.Remove(forge.UIPIDPath(dir))

	srv := httpServer(s)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		_ = shutdown(srv, s)
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
	mux.HandleFunc("GET /api/track", s.handleTrack)
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("POST /api/track/{ws}/inc", s.handleTrackInc)
	mux.HandleFunc("GET /api/term/{ws}/{kind}/stream", s.handleTermStream)
	mux.HandleFunc("POST /api/term/{ws}/{kind}/input", s.handleTermInput)
	mux.HandleFunc("POST /api/term/{ws}/{kind}/resize", s.handleTermResize)
	// The local shell belongs to no workspace, so it gets paths without one.
	mux.HandleFunc("GET /api/term/local/stream", s.handleLocalTermStream)
	mux.HandleFunc("POST /api/term/local/input", s.handleLocalTermInput)
	mux.HandleFunc("POST /api/term/local/resize", s.handleLocalTermResize)
	mux.HandleFunc("POST /api/ws/{ws}/stop", s.handleStop)
	mux.HandleFunc("POST /api/ws/{ws}/restart", s.handleRestart)
	mux.HandleFunc("POST /api/ws/{ws}/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("GET /api/fs/{ws}/list", s.handleFsList)
	mux.HandleFunc("GET /api/fs/{ws}/read", s.handleFsRead)
	mux.HandleFunc("GET /api/ports/{ws}", s.handlePorts)
	mux.HandleFunc("POST /api/ports/{ws}/container", s.handleContainerAction)
	mux.HandleFunc("GET /api/hosts", s.handleHosts)
	mux.HandleFunc("GET /api/hosts/stats", s.handleHostStats)
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

// handleWorkspaces reports every workspace and the state of its Claude session.
//
// One SSH round trip per host answers for every workspace on it, because that is
// the shape of the question: whether Claude is up in a particular workspace is
// per-workspace, but whether the machine answers at all is per-SERVER, and it is
// the second one the browser asks about over and over while a connection is down.
// Twenty workspaces on one server are still one server's connectivity.
//
// Two mechanisms keep that from becoming a storm of handshakes:
//
//   - Concurrent callers share one call. This is not a cache; everyone still gets
//     a freshly-measured status.
//   - ?maxAge=<seconds> says "an answer this recent is good enough for me", so a
//     caller that is merely probing reuses the last one instead of paying for a
//     new round trip.
//
// maxAge is opt-in for exactly this reason: a status you are about to ACT on must
// be measured, not remembered. So the reconnect probe passes it and everything
// else — page load, and every refresh after a stop/start/restart — does not, and
// is answered by a real round trip.
func (s *server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.listWorkspacesShared(parseMaxAge(r.URL.Query().Get("maxAge")))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	if list == nil {
		list = []WorkspaceInfo{}
	}
	writeJSON(w, list)
}

// maxAgeCap bounds what a caller may ask to reuse. A probe wants roughly the
// reconnect loop's own floor — asking the server more often than it retries buys
// nothing — and beyond half a minute a "current" status stops being one.
const maxAgeCap = 30 * time.Second

func parseMaxAge(v string) time.Duration {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0 // absent, junk, or explicitly none: measure it
	}
	d := time.Duration(n) * time.Second
	return min(d, maxAgeCap)
}

// listWorkspacesShared runs ListWorkspaces, subject to two ways of not running it:
// a result younger than maxAge is reused, and a call already in flight is joined
// rather than duplicated. The first caller in does the work; the rest read its
// result.
func (s *server) listWorkspacesShared(maxAge time.Duration) ([]WorkspaceInfo, error) {
	s.wsMu.Lock()
	// Recent enough for this caller? Only successful calls are reusable — caching
	// a failure would report a server as unreachable for as long as the window
	// lasts, after it had already come back.
	if s.now == nil {
		s.now = time.Now
	}
	if maxAge > 0 && s.wsLast != nil && s.wsLast.err == nil && s.now().Sub(s.wsLastAt) < maxAge {
		c := s.wsLast
		s.wsMu.Unlock()
		return c.list, c.err
	}
	if c := s.wsInFlight; c != nil {
		// Signalled while still holding the lock: once this fires, the caller has
		// committed to waiting on c, so it can never start a second call — which is
		// what lets a test release the leader deterministically after N joins rather
		// than after a hopeful sleep. Nil in production.
		if s.onJoin != nil {
			s.onJoin()
		}
		s.wsMu.Unlock()
		<-c.done
		return c.list, c.err
	}
	c := &wsListCall{done: make(chan struct{})}
	s.wsInFlight = c
	s.wsMu.Unlock()

	c.list, c.err = s.deps.ListWorkspaces()

	// Clear before closing, so a caller woken by the close starts a fresh call
	// rather than joining the one that just finished.
	s.wsMu.Lock()
	s.wsInFlight = nil
	s.wsLast, s.wsLastAt = c, s.now()
	s.wsMu.Unlock()
	close(c.done)

	return c.list, c.err
}

// handleActivity returns each workspace's Claude attention state, keyed by name.
// The UI polls this on a short interval; a host we can't reach just contributes
// nothing, so a slow or down host dims its tabs rather than failing the request.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	// Polled every few seconds; a cached copy would leave attention marks stale.
	w.Header().Set("Cache-Control", "no-store")
	act := map[string]Activity{}
	if s.deps.WorkspaceActivity != nil {
		if a, err := s.deps.WorkspaceActivity(); err == nil && a != nil {
			act = a
		}
	}
	writeJSON(w, act)
}

// handleTrack returns each workspace's session tracking, keyed by name. Like
// handleActivity it is polled on a short interval and degrades quietly: a host we
// can't reach just doesn't update its clocks this round.
func (s *server) handleTrack(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tr := map[string]Track{}
	if s.deps.WorkspaceTrack != nil {
		if t, err := s.deps.WorkspaceTrack(); err == nil && t != nil {
			tr = t
		}
	}
	writeJSON(w, tr)
}

// handleUsage returns each workspace's Claude usage, keyed by name — the browser
// groups it by login itself, since that grouping is a matter of presentation and the
// per-workspace figures are wanted alongside it either way. Like handleActivity it is
// polled and degrades quietly: a host we can't reach leaves its workspaces reporting
// the sample they last gave, which each carries its own timestamp for.
func (s *server) handleUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	use := map[string]Usage{}
	if s.deps.WorkspaceUsage != nil {
		if u, err := s.deps.WorkspaceUsage(); err == nil && u != nil {
			use = u
		}
	}
	writeJSON(w, use)
}

// statsFresh is how long a measurement is handed to further callers instead of
// taking new ones. It exists to make N browser tabs cost what one costs: every
// open tab polls the servers panel on its own timer, and each miss is an SSH
// round trip per registered host.
//
// It is deliberately just under the browser's own poll interval (SERVERS_POLL_MS
// in app.js — the assets test holds the two together), so a second tab polling out
// of phase with the first is served the first one's answer, while a single tab
// still measures afresh every round rather than being shown the previous reading
// twice.
const statsFresh = 8 * time.Second

// handleHostStats reports every registered server's CPU, memory and disk usage.
// Like handleActivity it is polled and degrades quietly: a host that cannot be
// reached comes back as a row saying so, not as a failed request.
func (s *server) handleHostStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.deps.HostStats == nil {
		writeJSON(w, []HostStat{})
		return
	}
	stats, err := s.hostStatsShared()
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	if stats == nil {
		stats = []HostStat{}
	}
	writeJSON(w, stats)
}

// hostStatsShared runs HostStats, subject to the same two ways of not running it
// as listWorkspacesShared: a result younger than statsFresh is reused, and a call
// already in flight is joined rather than duplicated. Unlike the workspace list
// there is no opt-in — nothing acts on these numbers, they are only read, so
// "recent" is always good enough.
func (s *server) hostStatsShared() ([]HostStat, error) {
	s.statsMu.Lock()
	if s.now == nil {
		s.now = time.Now
	}
	// Only a successful call is reusable: caching a failure would keep reporting a
	// server as down for the rest of the window after it came back.
	if s.statsLast != nil && s.statsLast.err == nil && s.now().Sub(s.statsLastAt) < statsFresh {
		c := s.statsLast
		s.statsMu.Unlock()
		return c.stats, c.err
	}
	if c := s.statsInFlight; c != nil {
		if s.onStatsJoin != nil {
			s.onStatsJoin()
		}
		s.statsMu.Unlock()
		<-c.done
		return c.stats, c.err
	}
	c := &statsCall{done: make(chan struct{})}
	s.statsInFlight = c
	s.statsMu.Unlock()

	c.stats, c.err = s.deps.HostStats()

	s.statsMu.Lock()
	s.statsInFlight = nil
	s.statsLast, s.statsLastAt = c, s.now()
	s.statsMu.Unlock()
	close(c.done)

	return c.stats, c.err
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
