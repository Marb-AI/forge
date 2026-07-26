package forge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Marb-AI/forge/internal/proc"
)

// Forge runs two long-lived processes on this machine: the tunnel supervisor and
// the browser UI daemon. Starting one, finding it and stopping it is the core's
// business rather than a front end's — `forge ui stop` and the UI's own idea of
// whether it is running have to mean the same thing, down to which pidfile is
// read and what a stale one counts as.
//
// A daemon is this binary re-executed with a hidden argument and detached from
// the terminal that launched it. That is a laptop's shape of it, and the one
// Forge ships today; a desktop shell will start the same core in-process
// instead. Keeping the shape behind these operations is what makes that a change
// here rather than in every caller.

// The hidden subcommands the detached daemons re-exec themselves with. The
// spawner is here and the dispatcher that answers them is in the CLI, so they
// are named once — a rename on one side alone would produce a daemon that starts
// and immediately prints the usage text.
const (
	RunSupervisorArg = "__run-supervisor"
	RunUIArg         = "__run-ui"
)

// UIPIDPath returns the UI daemon's pidfile location, sibling to the
// supervisor's. It means "bound and serving": the daemon writes it only after
// winning the port, which is what lets a start wait on it instead of opening a
// browser at an address nothing is listening on.
func UIPIDPath(dir string) string { return filepath.Join(dir, "ui.pid") }

// UITokenPath returns the session token's location. The daemon mints and writes
// it; whoever wants the URL reads it back, rather than assuming a token it did
// not see minted.
func UITokenPath(dir string) string { return filepath.Join(dir, "ui.token") }

// daemonPID reads a pidfile and reports the pid if that process is alive. A
// missing or stale pidfile — the daemon was killed and never cleaned up — reads
// as not running, which is the only reading that lets a replacement start.
func daemonPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !proc.Alive(pid) {
		return 0, false // stale
	}
	return pid, true
}

// startDetached re-execs this binary with arg as a background process that
// outlives the launching shell, logging to dir/logName. Shared by both daemons,
// which differ only in the argument and the log they write.
func startDetached(dir, logName, arg string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, logName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, arg)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = proc.DetachAttr() // detach from this terminal
	if err := cmd.Start(); err != nil {
		return err
	}
	// Do not Wait: let it outlive us. It writes its own pidfile on startup.
	return cmd.Process.Release()
}

// awaitPID waits for a pidfile to start (running) or stop (!running) naming a
// live process, and reports whether that happened within timeout.
//
// Both directions matter. Waiting for one to appear turns "the port is already
// taken" into an error at the moment of starting, instead of a dead URL; waiting
// for one to go is what makes `stop && start` work, since a replacement cannot
// bind a port the old daemon has not released yet.
func awaitPID(path string, running bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := daemonPID(path); ok == running {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, ok := daemonPID(path)
	return ok == running
}
