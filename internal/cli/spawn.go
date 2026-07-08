package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/supervisor"
)

// runSupervisorArg is the hidden subcommand the detached daemon re-execs itself
// with. `forge spawn` launches `forge __run-supervisor` in the background.
const runSupervisorArg = "__run-supervisor"

// spawnCmd ensures the tunnel supervisor is running. It is idempotent: if a
// live supervisor is already present it does nothing, so it is safe to call
// from a shell rc on every new terminal.
func spawnCmd(args []string) int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	if pid, ok := supervisorPID(dir); ok {
		fmt.Printf("supervisor already running (pid %d)\n", pid)
		return 0
	}
	if err := startSupervisorDetached(dir); err != nil {
		return fail("%v", err)
	}
	fmt.Println("supervisor started")
	return 0
}

// runSupervisor is the foreground body of the detached daemon process. It loads
// config and blocks in supervisor.Run until signalled.
func runSupervisor(_ []string) int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	if err := supervisor.Run(dir, cfg); err != nil {
		return fail("%v", err)
	}
	return 0
}

// startSupervisorDetached launches this binary again as a detached background
// process running the supervisor, with output going to ~/.forge/forge.log.
func startSupervisorDetached(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "forge.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, runSupervisorArg)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from this terminal
	if err := cmd.Start(); err != nil {
		return err
	}
	// Do not Wait: let it outlive us. It writes its own pidfile in Run().
	return cmd.Process.Release()
}

// supervisorPID returns the running supervisor's pid, if any. A stale pidfile
// (process gone) is treated as not-running.
func supervisorPID(dir string) (int, bool) {
	data, err := os.ReadFile(supervisor.PIDPath(dir))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false // stale
	}
	return pid, true
}

// stopSupervisor signals the running supervisor to shut down. Returns false if
// none was running.
func stopSupervisor(dir string) (bool, error) {
	pid, ok := supervisorPID(dir)
	if !ok {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	return true, nil
}
