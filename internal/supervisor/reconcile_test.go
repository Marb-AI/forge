package supervisor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

func block(start, size int) *agentproto.PortBlock {
	return &agentproto.PortBlock{Start: start, Size: size}
}

func testConfig() *config.Config {
	return &config.Config{
		Hosts: map[string]*config.Host{
			"srv":  {Alias: "srv", User: "root", Addr: "1.2.3.4", Port: 22},
			"srv2": {Alias: "srv2", User: "root", Addr: "5.6.7.8", Port: 22},
		},
		Ports:      map[string]map[string][]int{},
		Workspaces: map[string]string{"crm": "srv", "shop": "srv2"},
	}
}

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	dir := t.TempDir()
	// Its own directory, and its own store in it: the supervisor writes the config
	// back on every poll, so a test that shared one would be writing over whatever
	// else is there — and before the store existed, over the developer's own.
	return &Supervisor{
		dir:     dir,
		store:   config.NewFileStore(dir),
		state:   map[key]*TunnelStatus{},
		workers: map[key]*worker{},
	}
}

func TestObserveAllTunnelsOnlyInsideTheBlock(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()

	observe := func(h *config.Host) (map[string]agentproto.WorkspacePorts, error) {
		if h.Alias != "srv" {
			return map[string]agentproto.WorkspacePorts{}, nil
		}
		return map[string]agentproto.WorkspacePorts{
			"crm": {
				Block: block(16000, 100),
				Ports: []agentproto.Port{
					{Name: "web", Host: 16000, Running: true},
					{Name: "api", Host: 16001, Running: false}, // stopped: still tunnelled
					// Outside the block. Reported so the UI can point at it, never
					// carried — the workspace was told its block is what reaches the
					// laptop, and forwarding more would make that false.
					{Name: "stray", Host: 3000, Running: true},
				},
			},
		}, nil
	}

	want, answered := s.observeAll(cfg, observe)
	if !answered["srv"] {
		t.Fatal("srv answered; it should be in scope")
	}
	if !want[key{"srv", "crm", 16000}] || !want[key{"srv", "crm", 16001}] {
		t.Errorf("in-block ports missing: %v", want)
	}
	if want[key{"srv", "crm", 3000}] {
		t.Errorf("a port outside the block was tunnelled: %v", want)
	}
}

// A workspace this client did not create, or one with no block, is not ours to
// tunnel — the same rule every other command follows.
func TestObserveAllSkipsForeignAndBlocklessWorkspaces(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()

	observe := func(h *config.Host) (map[string]agentproto.WorkspacePorts, error) {
		if h.Alias != "srv" {
			return map[string]agentproto.WorkspacePorts{}, nil
		}
		return map[string]agentproto.WorkspacePorts{
			"crm":       {Block: nil, Ports: []agentproto.Port{{Host: 16000}}},
			"colleague": {Block: block(16100, 100), Ports: []agentproto.Port{{Host: 16100}}},
		}, nil
	}

	want, answered := s.observeAll(cfg, observe)
	if len(answered) == 0 {
		t.Fatal("a host answered")
	}
	if len(want) != 0 {
		t.Errorf("want = %v, expected nothing tunnelled", want)
	}
}

// If NO host answers, the caller must keep what it has. Tearing every tunnel down
// because the laptop lid was shut for a moment is the worst thing this loop could
// do — the tunnels would come back, but every connection through them dies.
func TestObserveAllReportsWhenNobodyAnswered(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()

	fail := func(*config.Host) (map[string]agentproto.WorkspacePorts, error) {
		return nil, errUnreachable{}
	}
	if _, answered := s.observeAll(cfg, fail); len(answered) != 0 {
		t.Errorf("no host answered; scope should be empty, got %v", answered)
	}

	// One host answering is enough to act on: the answer covers its own workspaces
	// and the silent host's tunnels are left alone by reconcile, not removed.
	partial := func(h *config.Host) (map[string]agentproto.WorkspacePorts, error) {
		if h.Alias == "srv2" {
			return nil, errUnreachable{}
		}
		return map[string]agentproto.WorkspacePorts{
			"crm": {Block: block(16000, 100), Ports: []agentproto.Port{{Host: 16000}}},
		}, nil
	}
	want, answered := s.observeAll(cfg, partial)
	if !answered["srv"] {
		t.Fatal("srv answered; it should be in scope")
	}
	if answered["srv2"] {
		t.Error("srv2 was unreachable; it must not be in scope")
	}
	if !want[key{"srv", "crm", 16000}] {
		t.Errorf("want = %v", want)
	}
}

type errUnreachable struct{}

func (errUnreachable) Error() string { return "unreachable" }

func TestCacheRoundTrip(t *testing.T) {
	s := newTestSupervisor(t)
	s.cache(map[key]bool{
		{"srv", "crm", 16001}:   true,
		{"srv", "crm", 16000}:   true,
		{"srv2", "shop", 16100}: true,
	}, map[string]bool{"srv": true, "srv2": true})

	cfg, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Ports["srv"]["crm"]; len(got) != 2 || got[0] != 16000 || got[1] != 16001 {
		t.Errorf("crm ports = %v, want sorted [16000 16001]", got)
	}
	if got := cfg.Ports["srv2"]["shop"]; len(got) != 1 || got[0] != 16100 {
		t.Errorf("shop ports = %v", got)
	}

	// And back out again: this is what a supervisor started while a host is
	// unreachable puts up, instead of nothing.
	cfg.Hosts = testConfig().Hosts
	want := cachedTunnels(cfg)
	if !want[key{"srv", "crm", 16000}] || !want[key{"srv2", "shop", 16100}] {
		t.Errorf("cached tunnels = %v", want)
	}
}

// Forwards for a host that has since been removed must not become tunnels.
func TestCachedTunnelsSkipsUnknownHosts(t *testing.T) {
	cfg := testConfig()
	cfg.Ports = map[string]map[string][]int{
		"srv":     {"crm": {16000}},
		"deleted": {"ghost": {16100}},
	}
	want := cachedTunnels(cfg)
	if !want[key{"srv", "crm", 16000}] {
		t.Error("live host's tunnel missing")
	}
	if want[key{"deleted", "ghost", 16100}] {
		t.Error("removed host's forwards should not be tunnelled")
	}
}

func TestBusyDetailAlwaysSaysSomethingUseful(t *testing.T) {
	// Port 0 is never listening, so the lsof lookup finds nothing — the message
	// must still state the fact rather than come out half-written.
	got := busyDetail(0)
	if got == "" {
		t.Fatal("empty detail")
	}
	if !strings.Contains(got, "0") || !strings.Contains(got, "in use") {
		t.Errorf("detail = %q", got)
	}
}

// reconcile's bookkeeping, without letting a tunnel actually run: an
// already-cancelled context makes supervise return at once, so what is under test
// is which workers get added, which get removed, and whether their status rows go
// with them.
func TestReconcileAddsAndRemovesWorkers(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := key{"srv", "crm", 16000}
	b := key{"srv", "crm", 16001}

	s.reconcile(ctx, cfg, map[key]bool{a: true, b: true}, allHosts(cfg))
	if len(s.workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(s.workers))
	}

	// Reconciling to the same set must not touch what is already running: this
	// runs every few seconds, and rebuilding tunnels on a timer would drop every
	// connection through them.
	before := s.workers[a]
	s.reconcile(ctx, cfg, map[key]bool{a: true, b: true}, allHosts(cfg))
	if s.workers[a] != before {
		t.Error("an unchanged tunnel was replaced")
	}

	// Dropping one leaves the other alone, and takes its status row with it — a
	// tunnel nobody wants should vanish from `forwarding status`, not linger as a
	// row that cannot be explained.
	s.reconcile(ctx, cfg, map[key]bool{a: true}, allHosts(cfg))
	if len(s.workers) != 1 || s.workers[a] == nil {
		t.Fatalf("workers = %v, want only %v", s.workers, a)
	}
	if _, ok := s.state[b]; ok {
		t.Error("removed tunnel left a status row behind")
	}

	s.reconcile(ctx, cfg, map[key]bool{}, allHosts(cfg))
	if len(s.workers) != 0 || len(s.state) != 0 {
		t.Errorf("workers = %v, state = %v; want both empty", s.workers, s.state)
	}
}

// A tunnel for a host that is no longer registered must not be started.
func TestReconcileSkipsUnknownHost(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.reconcile(ctx, cfg, map[key]bool{{"gone", "ghost", 16000}: true}, allHosts(cfg))
	if len(s.workers) != 0 {
		t.Errorf("workers = %v, want none", s.workers)
	}
}

// localPortHolder against a real listener. The parsing is platform-specific
// (lsof, then ps), so asserting it on made-up output would prove nothing about the
// machine it has to work on.
func TestLocalPortHolderNamesARealListener(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("no lsof")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got := localPortHolder(port)
	if got == "" {
		t.Fatalf("nothing found holding port %d, which this test is holding", port)
	}
	// It must name this process — the whole value of the message is that it says
	// what to kill, not merely that something is there.
	if !strings.Contains(got, strconv.Itoa(os.Getpid())) {
		t.Errorf("holder = %q, want the test's own pid %d", got, os.Getpid())
	}
	if !strings.Contains(busyDetail(port), got) {
		t.Errorf("busyDetail(%d) does not carry the holder %q", port, got)
	}
}

// Nothing is listening, so this must come back empty rather than naming whatever
// lsof printed last.
func TestLocalPortHolderOnAFreePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // now free

	if got := localPortHolder(port); got != "" {
		t.Errorf("holder = %q on a free port", got)
	}
}

// A host that did not answer must keep its tunnels. Absence from `want` means it
// was not asked, not that its ports went away — and tearing them down is the one
// failure this loop cannot have, because every connection through them dies.
func TestReconcileLeavesUnansweredHostsAlone(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	onSrv := key{"srv", "crm", 16000}
	onSrv2 := key{"srv2", "shop", 16100}
	s.reconcile(ctx, cfg, map[key]bool{onSrv: true, onSrv2: true}, allHosts(cfg))
	if len(s.workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(s.workers))
	}

	// srv answers with nothing; srv2 is silent and out of scope.
	s.reconcile(ctx, cfg, map[key]bool{}, map[string]bool{"srv": true})
	if s.workers[onSrv] != nil {
		t.Error("srv answered with no ports; its tunnel should have stopped")
	}
	if s.workers[onSrv2] == nil {
		t.Error("srv2 was silent; its tunnel must survive")
	}
	if _, ok := s.state[onSrv2]; !ok {
		t.Error("a surviving tunnel lost its status row")
	}
}

// The cache is per host for the same reason: overwriting it wholesale would erase
// the last known ports of the very host the cache exists for — one that is down.
func TestCacheKeepsUnansweredHostsEntries(t *testing.T) {
	s := newTestSupervisor(t)
	s.cache(map[key]bool{
		{"srv", "crm", 16000}:   true,
		{"srv2", "shop", 16100}: true,
	}, map[string]bool{"srv": true, "srv2": true})

	// Now only srv answers, and it publishes nothing.
	s.cache(map[key]bool{}, map[string]bool{"srv": true})

	cfg, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Ports["srv"]; ok {
		t.Errorf("srv answered with nothing; its stale entry should be gone: %v", cfg.Ports)
	}
	if got := cfg.Ports["srv2"]["shop"]; len(got) != 1 || got[0] != 16100 {
		t.Errorf("srv2 was silent; its cached ports must survive, got %v", cfg.Ports)
	}
}

// A tunnel that has been stopped must not reappear in the status file. Its
// goroutine can still be unwinding — and the "up after two seconds" timer can
// already be running — when reconcile drops it.
func TestSetIgnoresStoppedTunnels(t *testing.T) {
	s := newTestSupervisor(t)
	cfg := testConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	k := key{"srv", "crm", 16000}
	s.reconcile(ctx, cfg, map[key]bool{k: true}, allHosts(cfg))
	s.reconcile(ctx, cfg, map[key]bool{}, allHosts(cfg))

	// What a late write from the dying goroutine looks like.
	s.set(k, StateUp, "")
	if _, ok := s.state[k]; ok {
		t.Error("a stopped tunnel wrote itself back into the status file")
	}
}
