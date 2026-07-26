package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Marb-AI/forge/internal/sshx"
)

// The supervising loop, with a tunnel handed in rather than an ssh process
// started.
//
// These are the decisions the package exists to make — which failures are worth
// retrying, which one is not, and which one is about this machine rather than the
// server — and none of them could be reached in a test while a tunnel was a
// process: exercising them meant an ssh binary, a reachable host and a key it
// would refuse. What was tested instead was the string matching that fed them,
// which is now the transport's business.

// stubTunnel is a tunnel that carries nothing and stops when it is told to, or
// when the test decides its far end went away.
type stubTunnel struct {
	stopped chan struct{}
	once    sync.Once
}

func (s *stubTunnel) Wait() error { <-s.stopped; return nil }

func (s *stubTunnel) Close() error {
	s.once.Do(func() { close(s.stopped) })
	return nil
}

// stubBackend hands out tunnels, or the failure a test wants the loop to meet.
type stubBackend struct {
	mu sync.Mutex
	// err is what every Forward fails with; nil means hand out a live tunnel.
	err    error
	asked  int
	opened []*stubTunnel
}

func (b *stubBackend) Name() string                        { return "stub" }
func (b *stubBackend) Run(sshx.Target, sshx.Command) error { return nil }

func (b *stubBackend) Open(sshx.Target, sshx.Shell) (sshx.Terminal, error) {
	return nil, errors.New("this backend opens no terminals")
}

func (b *stubBackend) Forward(_ sshx.Target, _, _ int) (sshx.Tunnel, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked++
	if b.err != nil {
		return nil, b.err
	}
	tun := &stubTunnel{stopped: make(chan struct{})}
	b.opened = append(b.opened, tun)
	return tun, nil
}

func (b *stubBackend) attempts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asked
}

// lastTunnel is the most recent tunnel handed out, once there is one.
func (b *stubBackend) lastTunnel(t *testing.T) *stubTunnel {
	t.Helper()
	var tun *stubTunnel
	until(t, "a tunnel to be opened", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		if len(b.opened) == 0 {
			return false
		}
		tun = b.opened[len(b.opened)-1]
		return true
	})
	return tun
}

// useStub points the transport at a backend for one test, and puts the process
// back to choosing for itself afterwards.
func useStub(t *testing.T, b *stubBackend) *stubBackend {
	t.Helper()
	sshx.Use(b)
	t.Cleanup(func() { sshx.Use(nil) })
	return b
}

// supervising starts one tunnel's loop and returns its key, a channel that closes
// when the loop gives up by itself, and the way to stop it.
func supervising(t *testing.T, k key) (<-chan struct{}, context.CancelFunc, *Supervisor) {
	t.Helper()
	s := newTestSupervisor(t)
	// A status row belongs to a supervised tunnel, so the loop's own reports land
	// only when reconcile has registered it — as it would have.
	s.workers[k] = &worker{cancel: func() {}, done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		s.supervise(ctx, testConfig().Hosts[k.host], k)
	}()
	return returned, cancel, s
}

// A key the server will not accept never becomes one it will. Retrying it is a
// loop that runs forever and achieves nothing, so it stops and says why — which
// is the one place the supervisor gives up on its own.
func TestAnAuthenticationFailureStopsTheTunnelForGood(t *testing.T) {
	b := useStub(t, &stubBackend{err: fmt.Errorf("%w: Permission denied (publickey).", sshx.ErrAuth)})
	k := key{"srv", "crm", 16000}

	returned, _, s := supervising(t, k)

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop is still retrying a key that will never be accepted")
	}
	if got := s.status(k); got == nil || got.State != StateError {
		t.Fatalf("state = %+v, want %s", got, StateError)
	}
	if n := b.attempts(); n != 1 {
		t.Errorf("the tunnel was attempted %d times, want once and no more", n)
	}
}

// A local port held by another program is not the server's fault and does not
// clear by itself. So it is its own state, it says what to kill, and it keeps
// trying — killing that program has to be all there is to do.
func TestABusyLocalPortIsNamedAndKeptRetrying(t *testing.T) {
	b := useStub(t, &stubBackend{err: fmt.Errorf("%w: cannot listen to port: 16000", sshx.ErrPortBusy)})
	k := key{"srv", "crm", 16000}

	returned, cancel, s := supervising(t, k)

	until(t, "the tunnel to be reported blocked", func() bool {
		st := s.status(k)
		return st != nil && st.State == StateBlocked
	})
	if got := s.status(k).Detail; got == "" {
		t.Error("a blocked tunnel says nothing about what is holding the port")
	}

	until(t, "the tunnel to be tried again", func() bool { return b.attempts() >= 2 })
	select {
	case <-returned:
		t.Fatal("the loop gave up on a port that clears the moment its holder does")
	default:
	}

	cancel()
	<-returned
}

// A tunnel that came up is reported up, and a tunnel that drops is put back —
// the reason this daemon exists, since neither `ssh -L` nor a connection of our
// own reconnects on its own.
func TestATunnelThatDropsIsPutBackUp(t *testing.T) {
	b := useStub(t, &stubBackend{})
	k := key{"srv", "crm", 16000}

	returned, cancel, s := supervising(t, k)

	tun := b.lastTunnel(t)
	until(t, "the tunnel to be reported up", func() bool {
		st := s.status(k)
		return st != nil && st.State == StateUp
	})

	// The far end goes away, as a rebooting server's does.
	tun.Close()

	until(t, "a replacement tunnel", func() bool { return b.attempts() >= 2 })

	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not stop when its context was cancelled")
	}
}

// status is one tunnel's row, read the way the status file writer reads it.
func (s *Supervisor) status(k key) *TunnelStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.state[k]; st != nil {
		row := *st
		return &row
	}
	return nil
}

// until polls for a condition and fails saying which one never came true. The
// loop under test works on timers — a second between retries, two before a tunnel
// counts as established — so waiting is the only way to watch it, and a bare wait
// on a condition that never arrives is a package timeout rather than a sentence.
func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited for %s and it never happened", what)
}
