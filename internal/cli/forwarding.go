package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/supervisor"
)

func forwardingCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge forwarding <start|stop|status>")
	}
	switch args[0] {
	case "start":
		return forwardingStart(args[1:])
	case "stop":
		return forwardingStop()
	case "status", "st":
		return forwardingStatus()
	default:
		return fail("unknown forwarding command %q", args[0])
	}
}

// forwardingStart restarts the supervisor, which then picks up whatever the hosts
// currently publish.
//
// It used to scan for ports itself and freeze the result into the config, which is
// why it had to be re-run every time a service was added. The supervisor now does
// that continuously, so there is nothing here to scan: a restart is only worth
// asking for when you would rather not wait out the poll.
func forwardingStart(_ []string) int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	if stopped, err := stopSupervisor(dir); err != nil {
		return fail("stop supervisor: %v", err)
	} else if stopped {
		waitForSupervisorExit(dir)
	}
	if err := startSupervisorDetached(dir); err != nil {
		return fail("start supervisor: %v", err)
	}
	fmt.Println("forwarding (re)started — tunnels follow what the hosts publish")
	return 0
}

func forwardingStop() int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	stopped, err := stopSupervisor(dir)
	if err != nil {
		return fail("%v", err)
	}
	if !stopped {
		fmt.Println("supervisor not running")
		return 0
	}
	waitForSupervisorExit(dir)
	supervisor.ClearStatus(dir)
	fmt.Println("forwarding stopped")
	return 0
}

func forwardingStatus() int {
	dir, err := config.Dir()
	if err != nil {
		return fail("%v", err)
	}
	pid, running := supervisorPID(dir)
	if !running {
		fmt.Println("supervisor not running (forge spawn to start)")
		return 0
	}
	st, err := supervisor.ReadStatus(dir)
	if err != nil {
		fmt.Printf("supervisor running (pid %d), no status yet\n", pid)
		return 0
	}
	fmt.Printf("supervisor running (pid %d)\n", pid)
	sort.Slice(st.Tunnels, func(i, j int) bool {
		if st.Tunnels[i].Workspace != st.Tunnels[j].Workspace {
			return st.Tunnels[i].Workspace < st.Tunnels[j].Workspace
		}
		return st.Tunnels[i].Port < st.Tunnels[j].Port
	})
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tPORT\tSTATE\tDETAIL")
	for _, t := range st.Tunnels {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", t.Workspace, t.Port, t.State, t.Detail)
	}
	return flush(w)
}

// waitForSupervisorExit gives a signalled supervisor a moment to release the
// pidfile before we start a replacement.
func waitForSupervisorExit(dir string) {
	for i := 0; i < 30; i++ {
		if _, ok := supervisorPID(dir); !ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
