package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Marb-AI/forge/forge"
)

func forwardingCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge forwarding <start|stop|status>")
	}
	switch args[0] {
	case "start":
		return forwardingStart()
	case "stop":
		return forwardingStop()
	case "status", "st":
		if hasBoolFlag(args[1:], "-q", "--quiet") {
			return forwardingRunning()
		}
		return forwardingStatus()
	default:
		return fail("unknown forwarding command %q", args[0])
	}
}

func forwardingStart() int {
	if err := forge.RestartForwarding(); err != nil {
		return fail("%v", err)
	}
	fmt.Println("forwarding (re)started — tunnels follow what the hosts publish")
	return 0
}

func forwardingStop() int {
	stopped, err := forge.StopForwarding()
	if err != nil {
		return fail("%v", err)
	}
	if !stopped {
		fmt.Println("supervisor not running")
		return 0
	}
	fmt.Println("forwarding stopped")
	return 0
}

// forwardingRunning is `forge forwarding status -q` — see uiRunning.
func forwardingRunning() int {
	f, err := forge.ForwardingStatus()
	if err != nil || !f.Running {
		return 1
	}
	return 0
}

func forwardingStatus() int {
	f, err := forge.ForwardingStatus()
	if err != nil {
		return fail("%v", err)
	}
	if !f.Running {
		fmt.Println("supervisor not running (forge spawn to start)")
		return 0
	}
	if !f.Reported {
		fmt.Printf("supervisor running (pid %d), no status yet\n", f.PID)
		return 0
	}
	fmt.Printf("supervisor running (pid %d)\n", f.PID)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tPORT\tSTATE\tDETAIL")
	for _, t := range f.Tunnels {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", t.Workspace, t.Port, t.State, t.Detail)
	}
	return flush(w)
}

// spawnCmd ensures the tunnel supervisor is running. Idempotent: a live
// supervisor is left alone, so it is safe to call from a shell rc on every new
// terminal.
func spawnCmd(_ []string) int {
	pid, already, err := forge.SpawnSupervisor()
	if err != nil {
		return fail("%v", err)
	}
	if already {
		fmt.Printf("supervisor already running (pid %d)\n", pid)
		return 0
	}
	fmt.Printf("supervisor started (pid %d)\n", pid)
	return 0
}
