package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/forge"
)

func portsCmd(args []string) int {
	if len(args) == 0 {
		return portsList()
	}
	switch args[0] {
	case "range":
		return portsRange(args[1:])
	case "assign":
		return portsAssign(args[1:])
	default:
		return fail("unknown ports command %q (want: range, assign)", args[0])
	}
}

// portsList prints the range and who holds which block. Blocks are the one piece
// of port state a human ever needs to look at: everything else about a port is
// derived from what is actually running.
func portsList() int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	r := cfg.PortRangeOr()
	fmt.Printf("range %d-%d, blocks of %d (%d blocks)\n\n", r.Start, r.End, r.Block, len(r.Blocks()))

	held, unreachable, err := forge.HeldBlocks()
	if err != nil {
		return fail("%v", err)
	}
	reserved := cfg.ActiveReservations(time.Now())
	if len(held) == 0 && len(unreachable) == 0 && len(reserved) == 0 {
		fmt.Println("no workspaces")
		return 0
	}

	sort.Slice(held, func(i, j int) bool {
		if held[i].Block == nil || held[j].Block == nil {
			return held[j].Block != nil // workspaces with no block sort last
		}
		return held[i].Block.Start < held[j].Block.Start
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tHOST\tPORTS")
	missing := 0
	for _, h := range held {
		ports := "(none — forge ports assign)"
		if h.Block != nil {
			ports = fmt.Sprintf("%d-%d", h.Block.Start, h.Block.End())
		} else {
			missing++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", h.Workspace, h.Host, ports)
	}
	// Reservations hold a block for a workspace that does not exist yet, so nothing
	// above accounts for them. Shown because one can hold a block for half an hour
	// after a creation died, and an invisible thing holding a block is the kind of
	// state you end up reading the source to explain.
	for _, res := range reserved {
		fmt.Fprintf(w, "%s\t%s\t%d-%d (being created)\n",
			res.Workspace, res.Host, res.Start, res.Start+r.Block-1)
	}
	if code := flush(w); code != 0 {
		return code
	}
	for _, alias := range unreachable {
		fmt.Fprintf(os.Stderr, "\n  %s: unreachable — its blocks are unknown\n", alias)
	}
	if missing > 0 {
		fmt.Printf("\n%d workspace(s) without a block — assign with: forge ports assign\n", missing)
	}
	return 0
}

// portsRange shows or sets the range Forge allocates from. What a new range is
// allowed to be — and why an existing block can never move into it — is
// forge.SetPortRange's business; this only reads the argument and reports.
func portsRange(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	block := 0
	var span string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--block="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--block="))
			if err != nil || n <= 0 {
				return fail("--block wants a positive number")
			}
			block = n
		case strings.HasPrefix(a, "-"):
			return fail("unknown flag %q", a)
		default:
			span = a
		}
	}

	if span == "" && block == 0 {
		r := cfg.PortRangeOr()
		fmt.Printf("%d-%d, blocks of %d (%d blocks)\n", r.Start, r.End, r.Block, len(r.Blocks()))
		return 0
	}

	next := cfg.PortRangeOr()
	if span != "" {
		start, end, err := parseSpan(span)
		if err != nil {
			return fail("%v", err)
		}
		next.Start, next.End = start, end
	}
	if block > 0 {
		next.Block = block
	}

	if err := forge.SetPortRange(next); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("range %d-%d, blocks of %d (%d blocks)\n", next.Start, next.End, next.Block, len(next.Blocks()))

	// Advisory, at the one moment the user can still act on it: this is when the
	// range is chosen, so anything already sitting in it is worth knowing about now
	// rather than as a failed tunnel weeks later. Never fatal — a busy port is one
	// tunnel's problem when it comes to it, not a reason to refuse a range.
	warnRangeBusy(next)
	return 0
}

// parseSpan reads "16000-30000".
func parseSpan(s string) (start, end int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want a range like 16000-30000, got %q", s)
	}
	if start, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
		return 0, 0, fmt.Errorf("bad range start %q", a)
	}
	if end, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
		return 0, 0, fmt.Errorf("bad range end %q", b)
	}
	if start < 1024 || end > 65535 || end <= start {
		return 0, 0, fmt.Errorf("range must be inside 1024-65535 and ascending")
	}
	return start, end, nil
}

// portsAssign gives a block to every workspace that has none, or to the one
// named. Whatever landed is printed even when the run then fails: each
// assignment is real and permanent, so a caller has to be told about it.
func portsAssign(args []string) int {
	only := ""
	if len(args) > 0 {
		only = args[0]
	}

	assigned, err := forge.AssignBlocks(only)
	for _, a := range assigned {
		fmt.Printf("  %s: %d-%d\n", a.Workspace, a.Block.Start, a.Block.End())
	}
	if err != nil {
		return fail("%v", err)
	}

	if len(assigned) == 0 {
		if only != "" {
			fmt.Printf("%s already has a block\n", only)
		} else {
			fmt.Println("every workspace already has a block")
		}
	}
	return 0
}

// warnRangeBusy reports anything already listening locally inside the range. It is
// a courtesy at the moment the range is chosen, not a gate: a single busy port is
// no reason to reject a span of thousands, and when it actually matters the tunnel
// for that one port says so precisely (see the supervisor's collision state).
func warnRangeBusy(r config.PortRange) {
	busy := listeningIn(r.Start, r.End)
	if len(busy) == 0 {
		return
	}
	parts := make([]string, len(busy))
	for i, p := range busy {
		parts[i] = strconv.Itoa(p)
	}
	fmt.Fprintf(os.Stderr,
		"\n  note: %s already in use on this machine — a workspace given those ports cannot tunnel them until whatever holds them stops\n",
		strings.Join(parts, " "))
}

// listeningIn returns the local ports in [lo,hi] something is listening on. Uses
// lsof, which is on macOS and Linux both; a missing or unhappy lsof yields nothing,
// because this only ever feeds a warning.
func listeningIn(lo, hi int) []int {
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
