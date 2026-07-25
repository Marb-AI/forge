// Package supervisor keeps a set of `ssh -L` tunnels alive. It is the one
// genuinely non-trivial piece of Forge: a bare `ssh -L` is a single foreground
// process that neither waits for a down server nor reconnects, so the client
// must supervise it.
//
// Design:
//   - one supervised ssh process PER PORT, so a single failure can't cascade;
//   - 1-second fixed retry with no backoff, for sub-second recovery;
//   - an authentication failure is terminal (retrying can't fix it), so that
//     tunnel stops and is reported instead of spamming forever;
//   - `-L` is lazy, so a workspace service being *down* does not break the
//     tunnel — a tunnel to something not currently listening costs nothing;
//   - the set is not fixed at startup: it is reconciled every few seconds against
//     what the hosts actually publish, so tunnels appear and disappear with the
//     containers behind them.
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/proc"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Tunnel states written to status.json.
const (
	StateUp       = "up"
	StateRetrying = "retrying"
	StateError    = "error" // terminal, e.g. auth failure
	// StateBlocked: the local port is held by something else on this machine. A
	// category of its own because it is neither of the others — the server is fine,
	// so it is not an error there, and it will not clear on its own the way a blip
	// does. It keeps retrying, so killing whatever holds the port heals it within a
	// second with nothing else to do; the detail says what to kill.
	StateBlocked = "blocked"
)

const retryInterval = 1 * time.Second

// pollInterval is how often the hosts are asked what they publish. Matched to the
// UI's own server poll: a container coming up is not something anyone is watching
// to the second, and each round is one SSH round trip per host.
const pollInterval = 10 * time.Second

// Observer asks one host what its workspaces publish. Injected rather than called
// directly because reaching the agent lives in the CLI package, which imports this
// one — the same shape the browser UI uses for the same reason.
type Observer func(host *config.Host) (map[string]agentproto.WorkspacePorts, error)

type key struct {
	host, workspace string
	port            int
}

// TunnelStatus is one entry in status.json.
type TunnelStatus struct {
	Host      string `json:"host"`
	Workspace string `json:"workspace"`
	Port      int    `json:"port"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}

// Status is the whole status.json document.
type Status struct {
	PID       int            `json:"pid"`
	UpdatedAt string         `json:"updated_at"`
	Tunnels   []TunnelStatus `json:"tunnels"`
}

// Supervisor owns the running tunnel workers and the status file.
type Supervisor struct {
	dir   string
	mu    sync.Mutex
	state map[key]*TunnelStatus
	// workers is the tunnel currently supervised for each key. Reconciliation adds
	// and removes entries here; each one owns a goroutine that lives until its
	// cancel is called.
	workers map[key]*worker
}

// worker is one supervised tunnel: the goroutine's off switch, and a channel that
// closes when it has actually stopped.
type worker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func statusPath(dir string) string { return filepath.Join(dir, "status.json") }

// PIDPath returns the supervisor pidfile location.
func PIDPath(dir string) string { return filepath.Join(dir, "forge.pid") }

// Run supervises the tunnels every workspace needs and blocks until the process is
// signalled (SIGINT/SIGTERM). It writes the pidfile on entry and removes it on
// exit. This is the body of the detached `forge spawn` daemon.
//
// The tunnel set is not fixed at startup. It is recomputed every pollInterval from
// what the hosts actually publish, so a container brought up on a server is
// reachable from the laptop a few seconds later with nothing run by hand — which is
// the entire point. Tunnels for ports that went away are stopped by the same pass.
//
// The last observation is cached in the config, so a laptop that starts with a host
// unreachable still puts up the tunnels it had last time rather than none at all.
// `-L` is lazy, so a tunnel to something not currently listening costs nothing.
func Run(dir string, cfg *config.Config, observe Observer) error {
	s := &Supervisor{
		dir:     dir,
		state:   map[key]*TunnelStatus{},
		workers: map[key]*worker{},
	}

	if err := os.WriteFile(PIDPath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(PIDPath(dir))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		cancel()
	}()

	// Start from the cache immediately: waiting for the first poll would leave a
	// freshly spawned supervisor with no tunnels for as long as the hosts take to
	// answer, and those are the tunnels the user already had a moment ago.
	s.reconcile(ctx, cfg, cachedTunnels(cfg), allHosts(cfg))

	go s.pollLoop(ctx, observe)
	go s.statusLoop(ctx)
	s.writeStatus() // initial snapshot

	// Block until signalled, even with zero tunnels: the supervisor is a stable
	// daemon so `spawn` is idempotent and `forwarding start` can reload it.
	<-ctx.Done()

	s.stopAll()
	s.writeStatus()
	return nil
}

// pollLoop asks every host what it publishes and reconciles the tunnel set to it.
//
// The config is re-read each round rather than held from startup, because it
// changes underneath: a workspace created in the browser while this is running must
// get its tunnels without the daemon being restarted.
func (s *Supervisor) pollLoop(ctx context.Context, observe Observer) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		cfg, err := config.Load()
		if err == nil {
			// Only the hosts that answered are in scope: a silent one's tunnels are
			// left running rather than read as "publishes nothing".
			if want, answered := s.observeAll(cfg, observe); len(answered) > 0 {
				s.reconcile(ctx, cfg, want, answered)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// observeAll asks each host with workspaces on it what they publish, and returns
// the tunnels that implies together with the set of hosts that actually answered.
//
// That second return is not bookkeeping — it is the scope of everything the caller
// may act on. A host that did not answer said nothing, which is not the same as
// saying it publishes nothing: acting on the difference would stop every tunnel to
// a server that was briefly unreachable, which is precisely the failure this loop
// must not have. Its tunnels are therefore left alone, and so is its cache.
//
// Only ports inside a workspace's own block are tunnelled. The workspace was told,
// in writing, that its block is what reaches the developer's machine; forwarding
// more than that would make the promise false and let two workspaces fight over the
// same local port again. A port published outside the block is still reported by
// the agent, so the UI can point at it — it just is not carried.
func (s *Supervisor) observeAll(cfg *config.Config, observe Observer) (want map[key]bool, answered map[string]bool) {
	hosts := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		hosts[alias] = true
	}

	want = map[key]bool{}
	answered = map[string]bool{}
	for alias := range hosts {
		host := cfg.Hosts[alias]
		if host == nil {
			continue
		}
		seen, err := observe(host)
		if err != nil {
			continue
		}
		answered[alias] = true
		for ws, wp := range seen {
			// Workspaces this client did not create are not ours to tunnel, the same
			// rule every other command follows.
			if cfg.Workspaces[ws] != alias || wp.Block == nil {
				continue
			}
			for _, p := range wp.Ports {
				if wp.Block.Contains(p.Host) {
					want[key{alias, ws, p.Host}] = true
				}
			}
		}
	}
	if len(answered) == 0 {
		return nil, nil
	}

	s.cache(want, answered)
	return want, answered
}

// cache records the tunnel set in the config so the next start has something to
// work from before the first poll answers.
//
// Only the hosts that answered are rewritten. Replacing the whole map would erase
// the last known ports of a host that happened to be unreachable this round — and
// the cache exists for exactly that host, so that a restart while it is still down
// puts its tunnels up rather than none.
func (s *Supervisor) cache(want map[key]bool, answered map[string]bool) {
	byHost := map[string]map[string][]int{}
	for k := range want {
		if byHost[k.host] == nil {
			byHost[k.host] = map[string][]int{}
		}
		byHost[k.host][k.workspace] = append(byHost[k.host][k.workspace], k.port)
	}
	for _, workspaces := range byHost {
		for _, ports := range workspaces {
			sort.Ints(ports)
		}
	}
	_ = config.Update(func(c *config.Config) error {
		if c.Ports == nil {
			c.Ports = map[string]map[string][]int{}
		}
		for alias := range answered {
			// Cleared then rewritten, so a host that answered with nothing loses its
			// stale entry rather than keeping ports it no longer publishes.
			delete(c.Ports, alias)
			if ws := byHost[alias]; len(ws) > 0 {
				c.Ports[alias] = ws
			}
		}
		return nil
	})
}

// allHosts is every registered host, the scope of a reconcile driven by the cache
// rather than by an observation — nothing is running yet, so nothing can be stopped
// by mistake, and being explicit beats a nil that quietly means "everything".
func allHosts(cfg *config.Config) map[string]bool {
	hosts := map[string]bool{}
	for alias := range cfg.Hosts {
		hosts[alias] = true
	}
	return hosts
}

// cachedTunnels is the last observed set, read back from the config.
func cachedTunnels(cfg *config.Config) map[key]bool {
	want := map[key]bool{}
	for alias, workspaces := range cfg.Ports {
		if cfg.Hosts[alias] == nil {
			continue // host was removed; skip its stale forwards
		}
		for ws, ports := range workspaces {
			for _, port := range ports {
				want[key{alias, ws, port}] = true
			}
		}
	}
	return want
}

// reconcile makes the running tunnels match want: start what is missing, stop what
// is no longer wanted, leave the rest strictly alone.
//
// Leaving the rest alone is the whole point. This runs every few seconds, and a
// pass that tore tunnels down and rebuilt them would drop every connection through
// them on a timer.
//
// scope limits which hosts want speaks for. A tunnel to a host outside it is never
// stopped, however absent from want it looks — absence there means the host was not
// asked or did not answer, not that the port went away.
func (s *Supervisor) reconcile(ctx context.Context, cfg *config.Config, want map[key]bool, scope map[string]bool) {
	s.mu.Lock()
	var start []key
	var stop []key
	for k := range want {
		if s.workers[k] == nil {
			start = append(start, k)
		}
	}
	for k := range s.workers {
		if scope[k.host] && !want[k] {
			stop = append(stop, k)
		}
	}
	s.mu.Unlock()

	for _, k := range stop {
		s.stop(k)
	}
	for _, k := range start {
		host := cfg.Hosts[k.host]
		if host == nil {
			continue
		}
		s.start(ctx, host, k)
	}
}

// start launches one tunnel's supervising goroutine.
func (s *Supervisor) start(ctx context.Context, host *config.Host, k key) {
	wctx, cancel := context.WithCancel(ctx)
	w := &worker{cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	if s.workers[k] != nil { // already running; a concurrent pass won the race
		s.mu.Unlock()
		cancel()
		return
	}
	s.workers[k] = w
	s.mu.Unlock()

	s.set(k, StateRetrying, "starting")
	go func() {
		defer close(w.done)
		s.supervise(wctx, host, k)
	}()
}

// stop ends one tunnel and forgets it, including its status line — a tunnel that is
// no longer wanted should vanish from `forwarding status` rather than linger as a
// row nobody can explain.
func (s *Supervisor) stop(k key) {
	// Dropped from workers first, under the lock: from here on set() is a no-op for
	// this key, so the goroutine cannot put the row back as it unwinds.
	s.mu.Lock()
	w := s.workers[k]
	delete(s.workers, k)
	delete(s.state, k)
	s.mu.Unlock()

	if w == nil {
		return
	}
	w.cancel()
	// Waited on, not fired and forgotten: the ssh process must be gone before
	// anything could start a replacement on the same local port.
	<-w.done
}

// stopAll ends every tunnel, on shutdown.
func (s *Supervisor) stopAll() {
	s.mu.Lock()
	workers := make([]*worker, 0, len(s.workers))
	for k, w := range s.workers {
		workers = append(workers, w)
		delete(s.workers, k)
	}
	s.mu.Unlock()

	for _, w := range workers {
		w.cancel()
	}
	for _, w := range workers {
		<-w.done
	}
}

// supervise runs one port's ssh tunnel, restarting it on exit until the context
// is cancelled or the failure is terminal (auth).
func (s *Supervisor) supervise(ctx context.Context, h *config.Host, k key) {
	target := sshx.WorkspaceTarget(h, k.workspace)
	args := target.LocalForwardArgs(k.port, k.port)

	// Whether the last failure was the local port being taken, so the lookup of
	// what holds it happens once per streak rather than once a second.
	blocked := false

	for {
		if ctx.Err() != nil {
			return
		}

		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.SysProcAttr = proc.ChildAttr()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		startErr := cmd.Start()
		if startErr != nil {
			s.set(k, StateRetrying, startErr.Error())
			if !sleep(ctx, retryInterval) {
				return
			}
			continue
		}

		// If it stays up briefly, consider it established.
		established := time.AfterFunc(2*time.Second, func() {
			s.set(k, StateUp, "")
		})

		waitErr := cmd.Wait()
		established.Stop()

		if ctx.Err() != nil {
			return // shutting down, not a failure
		}

		msg := strings.TrimSpace(stderr.String())
		if isAuthFailure(msg) {
			// Terminal: retrying a bad key never succeeds.
			s.set(k, StateError, "authentication failed — check the SSH key")
			return
		}

		if isPortBusy(msg) {
			// Not terminal — killing whatever holds the port makes the next retry
			// succeed, with nothing else to do. But it will not clear by itself, so
			// it says which process to kill rather than making the user find out.
			//
			// Looked up once per streak, not per retry: this loop runs every second,
			// and an lsof a second forever is a real cost for an answer that does not
			// change while the port stays blocked.
			if !blocked {
				blocked = true
				s.set(k, StateBlocked, busyDetail(k.port))
			}
			if !sleep(ctx, retryInterval) {
				return
			}
			continue
		}
		blocked = false

		detail := firstLine(msg)
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		s.set(k, StateRetrying, detail)

		if !sleep(ctx, retryInterval) {
			return
		}
	}
}

// isPortBusy reports whether ssh failed because the LOCAL port is taken. With
// ExitOnForwardFailure=yes it gives up immediately and says so.
func isPortBusy(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "cannot listen to port")
}

// busyDetail says what is holding a local port, so the message is something to act
// on rather than something to investigate. Falls back to the bare fact when lsof
// is missing or says nothing — which is still true, just less useful.
func busyDetail(port int) string {
	if who := localPortHolder(port); who != "" {
		return "port " + strconv.Itoa(port) + " is held on this machine by " + who + " — stop it and the tunnel comes up on its own"
	}
	return "port " + strconv.Itoa(port) + " is already in use on this machine"
}

// localPortHolder returns something like `node (pid 4821)` for whatever is
// listening on a local port.
func localPortHolder(port int) string {
	out, err := exec.Command("lsof", "-nP", "-t", "-sTCP:LISTEN", "-iTCP:"+strconv.Itoa(port)).Output()
	if err != nil {
		return ""
	}
	pid := firstLine(strings.TrimSpace(string(out)))
	if pid == "" {
		return ""
	}
	name, err := exec.Command("ps", "-o", "comm=", "-p", pid).Output()
	if err != nil {
		return "pid " + pid
	}
	n := strings.TrimSpace(string(name))
	if n == "" {
		return "pid " + pid
	}
	return filepath.Base(n) + " (pid " + pid + ")"
}

func isAuthFailure(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "permission denied") ||
		strings.Contains(s, "publickey") ||
		strings.Contains(s, "too many authentication failures")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sleep waits d or returns false if the context is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// set records a tunnel's state, but only while that tunnel is still supervised.
//
// The guard is what stops a removed tunnel reappearing in the status file as a row
// nothing will ever clear. A stopped worker does not fall silent the instant it is
// cancelled: its goroutine may be unwinding, and the timer that marks a tunnel
// "up" after two seconds can already be running when it is stopped, so both can
// write after reconcile has dropped the key. Whether the write lands before or
// after the delete is a race; whether it takes effect is not.
func (s *Supervisor) set(k key, state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers[k] == nil {
		return
	}
	s.state[k] = &TunnelStatus{
		Host: k.host, Workspace: k.workspace, Port: k.port,
		State: state, Detail: detail,
	}
}

func (s *Supervisor) statusLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.writeStatus()
		}
	}
}

func (s *Supervisor) writeStatus() {
	s.mu.Lock()
	tunnels := make([]TunnelStatus, 0, len(s.state))
	for _, t := range s.state {
		tunnels = append(tunnels, *t)
	}
	s.mu.Unlock()

	st := Status{PID: os.Getpid(), UpdatedAt: time.Now().Format(time.RFC3339), Tunnels: tunnels}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := statusPath(s.dir) + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, statusPath(s.dir))
	}
}

// ClearStatus removes a stale status file (used when no supervisor is running).
func ClearStatus(dir string) { _ = os.Remove(statusPath(dir)) }

// ReadStatus loads status.json for `forge forwarding status`.
func ReadStatus(dir string) (*Status, error) {
	data, err := os.ReadFile(statusPath(dir))
	if err != nil {
		return nil, err
	}
	st := &Status{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}
