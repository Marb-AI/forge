package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/forge"
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
// server and the ports Forge is configured to forward. It is advisory: paste the
// list to Claude when starting a new project so it can pick non-conflicting host
// ports. Forge does not allocate — it reports.
func showPorts(args []string) int {
	only := ""
	if len(args) > 0 {
		only = args[0]
	}
	hosts, err := forge.HostPortUse(only)
	if err != nil {
		return fail("%v", err)
	}
	if len(hosts) == 0 {
		fmt.Println("no hosts registered")
		return 0
	}
	for _, h := range hosts {
		if h.Note != "" {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", h.Alias, h.Note)
		}
		fmt.Printf("%s (%s)\n", h.Alias, h.Addr)
		if len(h.Ports) == 0 {
			fmt.Println("  (none)")
		} else {
			fmt.Printf("  %s\n", joinInts(h.Ports))
		}
	}
	return 0
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}
