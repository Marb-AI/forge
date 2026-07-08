package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

func showCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge show ports [host]")
	}
	switch args[0] {
	case "ports":
		return showPorts(args[1:])
	default:
		return fail("unknown show target %q (want: ports)", args[0])
	}
}

// showPorts prints, per host, the union of ports currently listening on the
// server (via `ss`) and the ports Forge is configured to forward. It is
// advisory: paste the list to Claude when starting a new project so it can pick
// non-conflicting host ports. Forge does not allocate — it reports.
func showPorts(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Println("no hosts registered")
		return 0
	}

	// Which hosts to report on.
	var aliases []string
	if len(args) > 0 {
		if cfg.Hosts[args[0]] == nil {
			return fail("no such host %q", args[0])
		}
		aliases = []string{args[0]}
	} else {
		for a := range cfg.Hosts {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
	}

	for _, alias := range aliases {
		host := cfg.Hosts[alias]
		used := map[int]bool{}

		// Ports actually listening on the server right now.
		out, err := sshx.Capture(sshx.AdminTarget(host).Args("ss", "-H", "-tln")...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: could not read listening ports (%v)\n", alias, err)
		} else {
			for _, p := range parseListeningPorts(string(out)) {
				used[p] = true
			}
		}

		// Ports Forge is configured to forward for this host (may be reserved
		// but not currently listening).
		for _, ports := range cfg.Ports[alias] {
			for _, p := range ports {
				used[p] = true
			}
		}

		ports := make([]int, 0, len(used))
		for p := range used {
			ports = append(ports, p)
		}
		sort.Ints(ports)

		fmt.Printf("%s (%s)\n", alias, host.Addr)
		if len(ports) == 0 {
			fmt.Println("  (none)")
		} else {
			fmt.Printf("  %s\n", joinInts(ports))
		}
	}
	return 0
}

// parseListeningPorts extracts local ports from `ss -H -tln` output. Each line's
// 4th field is the local Address:Port (e.g. "0.0.0.0:3000", "127.0.0.1:5432",
// "[::]:8080"); we take the port after the last colon.
func parseListeningPorts(out string) []int {
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := fields[3]
		colon := strings.LastIndex(local, ":")
		if colon < 0 {
			continue
		}
		if p, err := strconv.Atoi(local[colon+1:]); err == nil {
			seen[p] = true
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}
