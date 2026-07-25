package supervisor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
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
	return &Supervisor{dir: t.TempDir(), state: map[key]*TunnelStatus{}, workers: map[key]*worker{}}
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

	want, ok := s.observeAll(cfg, observe)
	if !ok {
		t.Fatal("a host answered; ok should be true")
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

	want, ok := s.observeAll(cfg, observe)
	if !ok {
		t.Fatal("ok should be true")
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
	if _, ok := s.observeAll(cfg, fail); ok {
		t.Error("no host answered; ok should be false")
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
	want, ok := s.observeAll(cfg, partial)
	if !ok {
		t.Fatal("one host answered; ok should be true")
	}
	if !want[key{"srv", "crm", 16000}] {
		t.Errorf("want = %v", want)
	}
}

type errUnreachable struct{}

func (errUnreachable) Error() string { return "unreachable" }

func TestCacheRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	s := newTestSupervisor(t)
	s.cache(map[key]bool{
		{"srv", "crm", 16001}:   true,
		{"srv", "crm", 16000}:   true,
		{"srv2", "shop", 16100}: true,
	})

	cfg, err := config.Load()
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

func TestIsPortBusy(t *testing.T) {
	busy := []string{
		"bind [127.0.0.1]:16104: Address already in use",
		"channel_setup_fwd_listener_tcpip: cannot listen to port: 16104",
	}
	for _, s := range busy {
		if !isPortBusy(s) {
			t.Errorf("isPortBusy(%q) = false", s)
		}
	}
	// A local collision must not be confused with the two failures that already
	// have their own handling: one is terminal, the other retries quietly.
	notBusy := []string{
		"Permission denied (publickey).",
		"ssh: connect to host 1.2.3.4 port 22: Connection refused",
		"",
	}
	for _, s := range notBusy {
		if isPortBusy(s) {
			t.Errorf("isPortBusy(%q) = true", s)
		}
	}
	if isAuthFailure(busy[0]) {
		t.Error("a busy local port must not read as an auth failure")
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

	s.reconcile(ctx, cfg, map[key]bool{a: true, b: true})
	if len(s.workers) != 2 {
		t.Fatalf("workers = %d, want 2", len(s.workers))
	}

	// Reconciling to the same set must not touch what is already running: this
	// runs every few seconds, and rebuilding tunnels on a timer would drop every
	// connection through them.
	before := s.workers[a]
	s.reconcile(ctx, cfg, map[key]bool{a: true, b: true})
	if s.workers[a] != before {
		t.Error("an unchanged tunnel was replaced")
	}

	// Dropping one leaves the other alone, and takes its status row with it — a
	// tunnel nobody wants should vanish from `forwarding status`, not linger as a
	// row that cannot be explained.
	s.reconcile(ctx, cfg, map[key]bool{a: true})
	if len(s.workers) != 1 || s.workers[a] == nil {
		t.Fatalf("workers = %v, want only %v", s.workers, a)
	}
	if _, ok := s.state[b]; ok {
		t.Error("removed tunnel left a status row behind")
	}

	s.reconcile(ctx, cfg, map[key]bool{})
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

	s.reconcile(ctx, cfg, map[key]bool{{"gone", "ghost", 16000}: true})
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
