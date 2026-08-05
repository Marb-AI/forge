package forge

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Marb-AI/forge/internal/proc"
	"github.com/Marb-AI/forge/internal/supervisor"
)

// The tunnel supervisor: the process that keeps an `ssh -L` alive for every port
// this client's workspaces publish. These are the four things anyone does to it —
// start it, restart it, stop it, ask what it is carrying.

// Tunnel is one forwarded port as the supervisor last reported it.
//
// The supervisor's own status type lives under internal/, where a front end
// outside this repository cannot name it; a status handed out here has to be
// nameable by whoever asked for it. Same reason PortBlock is the core's own type.
type Tunnel struct {
	Workspace string
	Port      int
	// State is the supervisor's vocabulary — up, blocked, failed. The same strings
	// the ports panel switches on.
	State string
	// Detail is why, when that is worth saying: what is holding the port locally,
	// or what the connection failed with.
	Detail string
}

// Forwarding is the supervisor's state: whether it is running, and what it says
// it is carrying.
type Forwarding struct {
	Running bool
	PID     int
	// Reported is false when the supervisor is up but has not written a status
	// yet — the first poll takes a moment. Distinct from carrying no tunnels,
	// which is a supervisor that looked and found nothing to forward.
	Reported bool
	Tunnels  []Tunnel
}

// SpawnSupervisor makes sure the supervisor is running and reports its pid,
// along with whether it was already up. Idempotent, so it is safe to call from a
// shell rc on every new terminal.
//
// "Is running" is meant literally: it returns once the supervisor has claimed
// its pidfile, so the pid is a real one and a daemon that died on startup is
// reported as the failure it is rather than as a successful spawn.
func SpawnSupervisor() (pid int, already bool, err error) {
	dir, err := StateDir()
	if err != nil {
		return 0, false, err
	}
	if pid, ok := daemonPID(supervisor.PIDPath(dir)); ok {
		return pid, true, nil
	}
	if err := startSupervisor(dir); err != nil {
		return 0, false, err
	}
	pid, _ = daemonPID(supervisor.PIDPath(dir))
	return pid, false, nil
}

// Tunnels is this process holding the port forwards itself, for a front end with
// no daemon behind it: a desktop shell, and a phone after it, where a detached
// process is either surprising or impossible.
//
// It may be holding nothing at all — see StartForwarding — and Stop is safe
// either way, which is what lets a caller keep one of these without asking which
// kind it got.
type Tunnels struct{ in *supervisor.Instance }

// Stop takes down whatever this process put up, and leaves alone whatever it
// did not. Idempotent.
func (t *Tunnels) Stop() {
	if t != nil && t.in != nil {
		t.in.Stop()
	}
}

// StartForwarding makes sure this machine's tunnels are up, without a daemon.
//
// A daemon is what `forge ui` uses and it is the right answer there: the tunnels
// serve the machine rather than the window, so they outlive the browser tab. An
// application is the other case. Nothing can be re-exec'd on a phone, and the
// bundle a desktop app ships in holds no `forge` to re-exec anyway — so the
// supervisor runs here, in this process, for as long as there is a window.
//
// If one is already running, it is left alone and this returns a handle that
// holds nothing. Two supervisors on one machine want the same local ports, and
// the second one to ask loses every tunnel to ErrPortBusy; the one already up is
// serving the same ports this one would, so there is nothing to gain by taking
// them from it — and it belongs to whoever started it, not to this window.
func StartForwarding() (*Tunnels, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	if _, running := daemonPID(supervisor.PIDPath(dir)); running {
		return &Tunnels{}, nil
	}

	// From here on nothing is running, so the status file — if a previous
	// supervisor left one — describes tunnels that do not exist. The ports panel
	// reads it without asking whether anyone is holding them (tunnelStates in
	// ports.go), which is only ever safe because whatever stops a supervisor
	// clears it. Failing to start one is the third way of having none, and it has
	// to clear it too: a caller that carries on with TunnelErr set — which is the
	// whole point of reporting rather than returning — would otherwise put a panel
	// on screen showing live tunnels for ports nothing answers on.
	fail := func(err error) (*Tunnels, error) {
		supervisor.ClearStatus(dir)
		return nil, err
	}

	st, err := Store()
	if err != nil {
		return fail(err)
	}
	in, err := supervisor.Start(st, ObservePorts)
	if err != nil {
		return fail(err)
	}
	return &Tunnels{in: in}, nil
}

// RestartForwarding stops the supervisor if it is up and starts a fresh one,
// which then picks up whatever the hosts currently publish.
//
// It used to scan for ports and freeze the result into the config, which is why
// it had to be re-run every time a service was added. The supervisor does that
// continuously now, so there is nothing here to scan: a restart is only worth
// asking for when you would rather not wait out the poll.
func RestartForwarding() error {
	dir, err := StateDir()
	if err != nil {
		return err
	}
	if stopped, err := stopSupervisor(dir); err != nil {
		return fmt.Errorf("stop supervisor: %w", err)
	} else if stopped {
		awaitPID(supervisor.PIDPath(dir), false, 3*time.Second)
	}
	if err := startSupervisor(dir); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	return nil
}

// StopForwarding signals the supervisor to shut down and waits for it to go,
// dropping the status it left behind. Reports false if none was running.
func StopForwarding() (bool, error) {
	dir, err := StateDir()
	if err != nil {
		return false, err
	}
	stopped, err := stopSupervisor(dir)
	if err != nil || !stopped {
		return false, err
	}
	awaitPID(supervisor.PIDPath(dir), false, 3*time.Second)
	// The status file describes tunnels that no longer exist. Left in place it
	// would be read as current by the next thing that looks.
	supervisor.ClearStatus(dir)
	return true, nil
}

// ForwardingStatus reports the supervisor's state and its tunnels, sorted by
// workspace and port — a stable order, so the same state reads the same way
// twice running.
func ForwardingStatus() (Forwarding, error) {
	dir, err := StateDir()
	if err != nil {
		return Forwarding{}, err
	}
	pid, running := daemonPID(supervisor.PIDPath(dir))
	if !running {
		return Forwarding{}, nil
	}
	f := Forwarding{Running: true, PID: pid}
	st, err := supervisor.ReadStatus(dir)
	if err != nil {
		return f, nil // up, but has not written anything yet
	}
	f.Reported = true
	for _, t := range st.Tunnels {
		f.Tunnels = append(f.Tunnels, Tunnel{
			Workspace: t.Workspace,
			Port:      t.Port,
			State:     t.State,
			Detail:    t.Detail,
		})
	}
	sort.Slice(f.Tunnels, func(i, j int) bool {
		if f.Tunnels[i].Workspace != f.Tunnels[j].Workspace {
			return f.Tunnels[i].Workspace < f.Tunnels[j].Workspace
		}
		return f.Tunnels[i].Port < f.Tunnels[j].Port
	})
	return f, nil
}

// RunSupervisor is the foreground body of the detached supervisor process: it
// blocks until signalled. Both the store and the observer are handed in from
// here, so neither where the config lives nor how a host is reached is something
// the supervisor decides for itself.
func RunSupervisor() error {
	st, err := Store()
	if err != nil {
		return err
	}
	return supervisor.Run(st, ObservePorts)
}

// startSupervisor launches the detached supervisor daemon and waits for it to
// claim its pidfile, which Run does before anything else.
//
// Waiting is what makes the answer worth anything. Launching a process only says
// the fork worked; a supervisor that exits immediately — a config it cannot read,
// a second one already holding the pidfile — would otherwise be reported as
// started, and the first sign of trouble would be tunnels that never come up.
func startSupervisor(dir string) error {
	if err := startDetached(dir, "forge.log", RunSupervisorArg); err != nil {
		return err
	}
	if !awaitPID(supervisor.PIDPath(dir), true, 3*time.Second) {
		return fmt.Errorf("the supervisor didn't come up — see %s", filepath.Join(dir, "forge.log"))
	}
	return nil
}

// stopSupervisor signals a running supervisor to shut down. Reports false if
// there was none.
func stopSupervisor(dir string) (bool, error) {
	pid, ok := daemonPID(supervisor.PIDPath(dir))
	if !ok {
		return false, nil
	}
	if err := proc.Terminate(pid); err != nil {
		return false, err
	}
	return true, nil
}
