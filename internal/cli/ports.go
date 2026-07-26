package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

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
	m, err := forge.PortBlocks()
	if err != nil {
		return fail("%v", err)
	}
	r := m.Range
	fmt.Printf("range %d-%d, blocks of %d (%d blocks)\n\n", r.Start, r.End, r.Block, len(r.Blocks()))

	if len(m.Held) == 0 && len(m.Unreachable) == 0 && len(m.Reserved) == 0 {
		fmt.Println("no workspaces")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tHOST\tPORTS")
	missing := 0
	for _, h := range m.Held {
		ports := "(none — forge ports assign)"
		if h.Block != nil {
			ports = fmt.Sprintf("%d-%d", h.Block.Start, h.Block.End())
		} else {
			missing++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", h.Workspace, h.Host, ports)
	}
	for _, res := range m.Reserved {
		fmt.Fprintf(w, "%s\t%s\t%d-%d (being created)\n",
			res.Workspace, res.Host, res.Start, res.Start+r.Block-1)
	}
	if code := flush(w); code != 0 {
		return code
	}
	for _, alias := range m.Unreachable {
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
		r, err := forge.PortRange()
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%d-%d, blocks of %d (%d blocks)\n", r.Start, r.End, r.Block, len(r.Blocks()))
		return 0
	}

	var start, end int
	if span != "" {
		var err error
		if start, end, err = parseSpan(span); err != nil {
			return fail("%v", err)
		}
	}
	r, err := forge.SetPortRange(start, end, block)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("range %d-%d, blocks of %d (%d blocks)\n", r.Start, r.End, r.Block, len(r.Blocks()))

	// Advisory, at the one moment the user can still act on it: this is when the
	// range is chosen, so anything already sitting in it is worth knowing about now
	// rather than as a failed tunnel weeks later. Never fatal — a busy port is one
	// tunnel's problem when it comes to it, not a reason to refuse a range.
	warnRangeBusy(r.Start, r.End)
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

// warnRangeBusy reports anything already listening locally inside the range.
func warnRangeBusy(start, end int) {
	busy := forge.BusyLocalPorts(start, end)
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
