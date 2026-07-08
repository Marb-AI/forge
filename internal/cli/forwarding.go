package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
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

// forwardingStart scans the Docker published ports of the target workspace(s),
// records them in config, and (re)launches the supervisor. Run it once after
// bringing services up, and again only when you add or remove a service.
func forwardingStart(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}

	targets := map[string]string{} // workspace -> host alias
	if len(args) > 0 {
		name := args[0]
		alias, ok := cfg.Workspaces[name]
		if !ok {
			return fail("unknown workspace %q", name)
		}
		targets[name] = alias
	} else {
		if len(cfg.Workspaces) == 0 {
			return fail("no workspaces known — create one first")
		}
		for name, alias := range cfg.Workspaces {
			targets[name] = alias
		}
	}

	for name, alias := range targets {
		host := cfg.Hosts[alias]
		if host == nil {
			fmt.Fprintf(os.Stderr, "  skip %s: host %q unknown\n", name, alias)
			continue
		}
		ports, err := scanDockerPorts(host, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  scan %s: %v\n", name, err)
			continue
		}
		cfg.SetPorts(alias, name, ports)
		fmt.Printf("  %s: %s\n", name, formatPorts(ports))
	}

	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}

	// Reload = stop the old supervisor, then spawn a fresh one with new config.
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
	fmt.Println("forwarding (re)started")
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

// scanDockerPorts asks Docker on the server (as the workspace user) which host
// ports the workspace's compose project publishes. Convention: the compose
// project name equals the workspace name.
func scanDockerPorts(host *config.Host, workspace string) ([]int, error) {
	target := sshx.WorkspaceTarget(host, workspace)
	label := "label=com.docker.compose.project=" + workspace
	out, err := sshx.Capture(target.Args(
		"docker", "ps", "--filter", label, "--format", "{{.Ports}}",
	)...)
	if err != nil {
		return nil, err
	}
	return parsePublishedPorts(string(out)), nil
}

// parsePublishedPorts extracts host ports from `docker ps` "Ports" output, e.g.
//
//	0.0.0.0:3000->3000/tcp, :::3000->3000/tcp
//	0.0.0.0:5173->5173/tcp
//
// Only published ports (those with a "host:PORT->" segment) are returned, deduped
// and sorted.
func parsePublishedPorts(out string) []int {
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		for _, seg := range strings.Split(line, ",") {
			seg = strings.TrimSpace(seg)
			arrow := strings.Index(seg, "->")
			if arrow < 0 {
				continue // unpublished port, skip
			}
			hostPart := seg[:arrow]
			colon := strings.LastIndex(hostPart, ":")
			if colon < 0 {
				continue
			}
			if p, err := strconv.Atoi(hostPart[colon+1:]); err == nil {
				seen[p] = true
			}
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return "(no published ports found)"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, " ")
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
