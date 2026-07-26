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
	"github.com/Marb-AI/forge/internal/agentproto"
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

	held, unreachable := heldBlocks(cfg)
	reserved := cfg.ActiveReservations(time.Now())
	if len(held) == 0 && len(unreachable) == 0 && len(reserved) == 0 {
		fmt.Println("no workspaces")
		return 0
	}

	sort.Slice(held, func(i, j int) bool {
		if held[i].block == nil || held[j].block == nil {
			return held[j].block != nil // workspaces with no block sort last
		}
		return held[i].block.Start < held[j].block.Start
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tHOST\tPORTS")
	missing := 0
	for _, h := range held {
		ports := "(none — forge ports assign)"
		if h.block != nil {
			ports = fmt.Sprintf("%d-%d", h.block.Start, h.block.End())
		} else {
			missing++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", h.workspace, h.alias, ports)
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

// portsRange shows or sets the range Forge allocates from.
//
// Setting it does not move any block that already exists — blocks are immutable,
// which is the property everything else here relies on. A new range only decides
// where the NEXT block comes from, so widening one is safe and narrowing one below
// existing blocks is refused rather than silently leaving workspaces outside it.
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
	if len(next.Blocks()) == 0 {
		return fail("range %d-%d holds no block of %d ports", next.Start, next.End, next.Block)
	}

	// Existing blocks were handed out under the old range and cannot move. A new
	// range that does not contain one of them would leave that workspace publishing
	// ports this client no longer considers its own.
	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		return fail("cannot check existing blocks: %s unreachable", strings.Join(unreachable, ", "))
	}
	for _, h := range held {
		if h.block == nil {
			continue
		}
		if h.block.Start < next.Start || h.block.End() > next.End {
			return fail("workspace %q holds %d-%d, which is outside %d-%d — blocks never move, so widen the range instead",
				h.workspace, h.block.Start, h.block.End(), next.Start, next.End)
		}
	}

	if err := config.Update(func(c *config.Config) error {
		c.PortRange = next
		return nil
	}); err != nil {
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

// portsAssign gives a block to every workspace that has none — the backfill for
// workspaces created before blocks existed. Idempotent: a workspace that already
// holds one is left alone, because a block that moved would break every port
// written into a repo under the old one.
func portsAssign(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	only := ""
	if len(args) > 0 {
		only = args[0]
		if _, ok := cfg.Workspaces[only]; !ok {
			return fail("unknown workspace %q", only)
		}
	}

	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		// Refuse rather than allocate blind. An unreachable host's blocks are
		// invisible, not absent, and handing out one it already holds is exactly the
		// collision the whole scheme exists to prevent — silently, and only noticed
		// once both workspaces are running.
		return fail("cannot reach %s — its blocks are unknown, so allocating now could hand out one it already holds",
			strings.Join(unreachable, ", "))
	}

	taken := takenBlocks(held, cfg.ActiveReservations(time.Now()))
	r := cfg.PortRangeOr()
	assigned := 0
	for _, h := range held {
		if h.block != nil || (only != "" && h.workspace != only) {
			continue
		}
		start, ok := nextFreeBlock(r, taken)
		if !ok {
			return fail("no free block left in %d-%d — widen it with: forge ports range", r.Start, r.End)
		}
		host := cfg.Hosts[h.alias]
		if err := forge.CallAgent(host, nil, "workspace-port-block",
			"--name", h.workspace,
			"--port-start", strconv.Itoa(start),
			"--port-size", strconv.Itoa(r.Block),
		); err != nil {
			return fail("%s: %v", h.workspace, err)
		}
		taken[start] = true
		assigned++
		fmt.Printf("  %s: %d-%d\n", h.workspace, start, start+r.Block-1)
	}

	if assigned == 0 {
		if only != "" {
			fmt.Printf("%s already has a block\n", only)
		} else {
			fmt.Println("every workspace already has a block")
		}
	}
	return 0
}

// holder is one workspace and the block it holds (nil for none).
type holder struct {
	workspace string
	alias     string
	block     *agentproto.PortBlock
}

// heldBlocks asks every host which blocks its workspaces hold, and returns them
// with the aliases of the hosts that could not be asked.
//
// It is deliberately a query across ALL hosts, not one: a block is unique across
// every server this client knows, because a workspace's host port doubles as the
// local port that reaches it. Two hosts allocating independently would each be
// right and still collide on the laptop tunnelling both.
//
// Only workspaces this client created are considered — cfg.Workspaces, the same
// rule every other command follows. A host may hold others; they are not ours to
// count, and their blocks (if any) belong to whoever made them.
func heldBlocks(cfg *config.Config) (held []holder, unreachable []string) {
	byHost := map[string][]string{}
	for ws, alias := range cfg.Workspaces {
		byHost[alias] = append(byHost[alias], ws)
	}
	aliases := make([]string, 0, len(byHost))
	for a := range byHost {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	for _, alias := range aliases {
		host := cfg.Hosts[alias]
		if host == nil {
			// A workspace pointing at a host that is not registered (a hand-edited
			// config; removing a host drops its workspaces with it). Unreachable, not
			// absent — skipping it silently would let its block be handed out twice.
			unreachable = append(unreachable, alias)
			continue
		}
		var res agentproto.ListResult
		if err := forge.CallAgent(host, &res, "workspace-list"); err != nil {
			unreachable = append(unreachable, alias)
			continue
		}
		blocks := map[string]*agentproto.PortBlock{}
		for _, w := range res.Workspaces {
			blocks[w.Name] = w.PortBlock
		}
		for _, ws := range byHost[alias] {
			held = append(held, holder{workspace: ws, alias: alias, block: blocks[ws]})
		}
	}
	return held, unreachable
}

// nextFreeBlock returns the lowest block position in r that nobody holds. Lowest
// rather than next-after-the-highest, so a deleted workspace's block is reused and
// the numbers stay small and memorable instead of drifting up forever.
func nextFreeBlock(r config.PortRange, taken map[int]bool) (int, bool) {
	for _, start := range r.Blocks() {
		if !taken[start] {
			return start, true
		}
	}
	return 0, false
}

// allocateBlock picks a block for a workspace about to be created and reserves it
// in the same breath. Same rules as the backfill, and the same refusal to guess
// past an unreachable host.
//
// The reservation is what makes concurrent creations safe. Reading the hosts is
// slow and creating the workspace is slower still — minutes, while Claude Code
// installs — so without one, everything started in that window picks the same
// "lowest free" block. Choosing and reserving happen inside a single atomic config
// update, so the second caller sees the first one's choice immediately instead of
// after the first has finished.
//
// The hosts are read BEFORE that update, never inside it: config.Update holds a
// lock that every other config write waits on, and an SSH round trip under it would
// stall the UI daemon for as long as the slowest host takes to answer.
func allocateBlock(cfg *config.Config, workspace, alias string) (*agentproto.PortBlock, error) {
	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		return nil, fmt.Errorf("cannot reach %s — its port blocks are unknown, and allocating without them risks handing out one twice",
			strings.Join(unreachable, ", "))
	}

	r := cfg.PortRangeOr()
	var block *agentproto.PortBlock
	err := config.Update(func(c *config.Config) error {
		taken := takenBlocks(held, c.ActiveReservations(time.Now()))
		start, ok := nextFreeBlock(r, taken)
		if !ok {
			return fmt.Errorf("no free port block left in %d-%d — widen it with: forge ports range", r.Start, r.End)
		}
		c.ReservePortBlock(workspace, alias, start, time.Now())
		block = &agentproto.PortBlock{Start: start, Size: r.Block}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return block, nil
}

// releaseBlock drops a workspace's reservation, once the workspace itself is the
// record of the block — or once creating it has failed.
func releaseBlock(workspace string) {
	// Best-effort: a reservation that outlives its purpose expires on its own, and
	// failing a creation over a bookkeeping write would be worse than the leak.
	_ = config.Update(func(c *config.Config) error {
		c.ReleasePortBlock(workspace)
		return nil
	})
}

// takenBlocks is every block position that is spoken for: held by a workspace that
// exists, or reserved for one being created.
func takenBlocks(held []holder, reserved []config.PortReservation) map[int]bool {
	taken := map[int]bool{}
	for _, h := range held {
		if h.block != nil {
			taken[h.block.Start] = true
		}
	}
	for _, r := range reserved {
		taken[r.Start] = true
	}
	return taken
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

// containerAction starts or stops one of a workspace's containers.
func containerAction(workspace, service, action string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host := cfg.HostFor(workspace)
	if host == nil {
		return fmt.Errorf("unknown workspace %q", workspace)
	}
	return forge.CallAgent(host, nil, "workspace-container",
		"--name", workspace, "--service", service, "--action", action)
}
