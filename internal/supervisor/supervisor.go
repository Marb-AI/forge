// Package supervisor keeps a set of `ssh -L` tunnels alive. It is the one
// genuinely non-trivial piece of Forge: a bare `ssh -L` is a single foreground
// process that neither waits for a down server nor reconnects, so the client
// must supervise it.
//
// Design (see docs/IMPLEMENTATION_PLAN.md §2.6):
//   - one supervised ssh process PER PORT, so a single failure can't cascade;
//   - 1-second fixed retry with no backoff, for sub-second recovery;
//   - an authentication failure is terminal (retrying can't fix it), so that
//     tunnel stops and is reported instead of spamming forever;
//   - `-L` is lazy, so a workspace service being *down* does not break the
//     tunnel — we forward the whole configured set unconditionally.
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Tunnel states written to status.json.
const (
	StateUp       = "up"
	StateRetrying = "retrying"
	StateError    = "error" // terminal, e.g. auth failure
)

const retryInterval = 1 * time.Second

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
}

func statusPath(dir string) string { return filepath.Join(dir, "status.json") }

// PIDPath returns the supervisor pidfile location.
func PIDPath(dir string) string { return filepath.Join(dir, "forge.pid") }

// Run builds the tunnel set from cfg and blocks, supervising every tunnel until
// the process is signalled (SIGINT/SIGTERM). It writes the pidfile on entry and
// removes it on exit. This is the body of the detached `forge spawn` daemon.
func Run(dir string, cfg *config.Config) error {
	s := &Supervisor{dir: dir, state: map[key]*TunnelStatus{}}

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

	var wg sync.WaitGroup
	for hostAlias, workspaces := range cfg.Ports {
		host := cfg.Hosts[hostAlias]
		if host == nil {
			continue // host was removed; skip its stale forwards
		}
		for ws, ports := range workspaces {
			for _, port := range ports {
				k := key{hostAlias, ws, port}
				s.set(k, StateRetrying, "starting")
				wg.Add(1)
				go func(h *config.Host, k key) {
					defer wg.Done()
					s.supervise(ctx, h, k)
				}(host, k)
			}
		}
	}

	// Periodically flush status so `forge forwarding status` sees fresh data.
	go s.statusLoop(ctx)
	s.writeStatus() // initial snapshot

	// Block until signalled, even with zero tunnels: the supervisor is a stable
	// daemon so `spawn` is idempotent and `forwarding start` can reload it.
	<-ctx.Done()

	wg.Wait()
	s.writeStatus()
	return nil
}

// supervise runs one port's ssh tunnel, restarting it on exit until the context
// is cancelled or the failure is terminal (auth).
func (s *Supervisor) supervise(ctx context.Context, h *config.Host, k key) {
	target := sshx.WorkspaceTarget(h, k.workspace)
	args := target.LocalForwardArgs(k.port, k.port)

	for {
		if ctx.Err() != nil {
			return
		}

		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

func (s *Supervisor) set(k key, state, detail string) {
	s.mu.Lock()
	s.state[k] = &TunnelStatus{
		Host: k.host, Workspace: k.workspace, Port: k.port,
		State: state, Detail: detail,
	}
	s.mu.Unlock()
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
