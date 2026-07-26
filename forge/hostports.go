package forge

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

// HostPorts is what one server has spoken for: every port listening on it right
// now, plus the ports this client is configured to forward from it — which may
// be reserved without anything listening yet.
type HostPorts struct {
	Alias string
	Addr  string
	Ports []int
	// Note is why the listening half is missing, when it is: a host that could not
	// be reached still has its forwarded ports worth showing, and a list that
	// quietly dropped the other half would read as "nothing is running there".
	Note string
}

// HostPortUse answers "which ports are taken on my servers" — the list you hand
// Claude when starting a project, so it picks ports that collide with nothing.
// One host when alias is given, every registered host when it is empty.
//
// Forge does not allocate here; it reports. Allocation is what blocks are for.
func HostPortUse(alias string) ([]HostPorts, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	var aliases []string
	if alias != "" {
		if cfg.Hosts[alias] == nil {
			return nil, fmt.Errorf("no such host %q", alias)
		}
		aliases = []string{alias}
	} else {
		for a := range cfg.Hosts {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
	}

	out := make([]HostPorts, 0, len(aliases))
	for _, a := range aliases {
		host := cfg.Hosts[a]
		hp := HostPorts{Alias: a, Addr: host.Addr}
		used := map[int]bool{}

		if listening, err := listeningPorts(host); err != nil {
			hp.Note = fmt.Sprintf("could not read listening ports (%v)", err)
		} else {
			for _, p := range listening {
				used[p] = true
			}
		}
		// The ports this client forwards for the host. A reserved one is not
		// listening and would otherwise look free to whoever asked.
		for _, ports := range cfg.Ports[a] {
			for _, p := range ports {
				used[p] = true
			}
		}

		for p := range used {
			hp.Ports = append(hp.Ports, p)
		}
		sort.Ints(hp.Ports)
		out = append(out, hp)
	}
	return out, nil
}

// listeningPorts asks a host what it is listening on.
func listeningPorts(host *config.Host) ([]int, error) {
	out, err := sshx.Capture(sshx.AdminTarget(host).Args("ss", "-H", "-tln")...)
	if err != nil {
		return nil, err
	}
	return parseListeningPorts(string(out)), nil
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
