package forge

import (
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// BusyLocalPorts reports which ports in [lo,hi] something on this machine is
// already listening on.
//
// It exists for one moment: choosing a port range. A workspace given ports that
// are already taken locally cannot tunnel them, and the only time anyone can
// cheaply act on that is while picking the span — afterwards it surfaces as one
// tunnel that will not come up, weeks later. It is advisory by construction and
// never a gate: a busy port is one tunnel's problem when it comes to it, not a
// reason to refuse a span of thousands.
func BusyLocalPorts(lo, hi int) []int {
	// lsof is on macOS and Linux both. A missing or unhappy lsof yields nothing,
	// because nothing here is worth failing an operation over.
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	return parseLsofPorts(string(out), lo, hi)
}

// parseLsofPorts pulls the local ports in [lo,hi] out of `lsof -nP -iTCP
// -sTCP:LISTEN` output, whose 9th field is the address ("*:16000",
// "127.0.0.1:16001", "[::1]:16002").
func parseLsofPorts(out string, lo, hi int) []int {
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		addr := fields[8]
		colon := strings.LastIndex(addr, ":")
		if colon < 0 {
			continue
		}
		p, err := strconv.Atoi(addr[colon+1:])
		if err != nil || p < lo || p > hi {
			continue
		}
		seen[p] = true
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}
