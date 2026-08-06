package forge

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
)

// PortBlock is the run of host ports a workspace owns — what its services may
// publish, and, because a block is unique across every server this client knows,
// the local ports those services arrive on.
//
// The core's own type rather than the agent's wire one: that lives under
// internal/, where a front end outside this repository cannot name it, and a
// block an operation hands back has to be nameable by whoever asked for it.
type PortBlock struct {
	Start int
	Size  int
}

// End is the last port of the block.
func (b PortBlock) End() int { return b.Start + b.Size - 1 }

// Holder is one workspace and the block it holds — nil for a workspace created
// before blocks existed, which is what `forge ports assign` backfills.
type Holder struct {
	Workspace string
	Host      string
	Block     *PortBlock
}

// Assignment is one workspace that was just given a block.
type Assignment struct {
	Workspace string
	Block     PortBlock
}

// BlockMap is the whole picture of port allocation on this client: the span
// blocks are cut from, who holds which one, which are held for a workspace being
// created, and which hosts could not be asked — whose blocks are therefore
// unknown, which is not the same as absent.
type BlockMap struct {
	Range config.PortRange
	// Held is sorted by block, with the workspaces that have none last: the
	// blockless are what `forge ports assign` exists to fix, and a list sorted by a
	// value half of it lacks has to put them somewhere on purpose.
	Held []Holder
	// Reserved holds blocks for workspaces that do not exist yet. Worth reporting
	// because a reservation outlives a creation that died, by half an hour — and an
	// invisible thing holding a block is the kind of state you end up reading the
	// source to explain.
	Reserved    []config.PortReservation
	Unreachable []string
}

// PortBlocks reports which block each of this client's workspaces holds, what is
// reserved, and which hosts could not be reached.
func PortBlocks() (BlockMap, error) {
	cfg, err := loadConfig()
	if err != nil {
		return BlockMap{}, err
	}
	held, unreachable := heldBlocks(cfg)
	sortHolders(held)
	return BlockMap{
		Range:       cfg.PortRangeOr(),
		Held:        held,
		Reserved:    cfg.ActiveReservations(time.Now()),
		Unreachable: unreachable,
	}, nil
}

// PortRange returns the span blocks are currently cut from, without asking any
// host anything — the cheap half of PortBlocks, for when the span is all that was
// asked about.
func PortRange() (config.PortRange, error) {
	cfg, err := loadConfig()
	if err != nil {
		return config.PortRange{}, err
	}
	return cfg.PortRangeOr(), nil
}

// sortHolders orders workspaces by the block they hold, and puts the ones with no
// block at the end — where they read as a list of what still needs one, under a
// table whose third column they are the only ones missing.
//
// The comparator this replaced said the same thing in its comment and did the
// opposite: `held[j].Block != nil` sorts the blockless FIRST, so `forge ports`
// opened with the workspaces that have no ports and buried the ones that do.
// It moved here verbatim, comment and all, and only then had to be read.
func sortHolders(held []Holder) {
	sort.Slice(held, func(i, j int) bool {
		if held[i].Block == nil || held[j].Block == nil {
			return held[i].Block != nil
		}
		return held[i].Block.Start < held[j].Block.Start
	})
}

// AssignBlocks gives a block to every workspace that has none — the backfill for
// workspaces created before blocks existed — or to just one, when only names it.
// Idempotent: a workspace that already holds a block is left alone, because a
// block that moved would break every port written into a repo under the old one.
//
// What it managed is returned even when it then fails. Each assignment is a round
// trip to a server, and one that failed does not undo the ones before it — a
// caller that dropped them on the floor would be hiding blocks that now exist.
func AssignBlocks(only string) ([]Assignment, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if only != "" {
		if _, ok := cfg.Workspaces[only]; !ok {
			return nil, fmt.Errorf("unknown workspace %q", only)
		}
	}

	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		// Refuse rather than allocate blind. An unreachable host's blocks are
		// invisible, not absent, and handing out one it already holds is exactly the
		// collision the whole scheme exists to prevent — silently, and only noticed
		// once both workspaces are running.
		return nil, fmt.Errorf("cannot reach %s — its blocks are unknown, so allocating now could hand out one it already holds",
			strings.Join(unreachable, ", "))
	}

	taken := takenBlocks(held, cfg.ActiveReservations(time.Now()))
	// One range per host, and one `taken` across all of them.
	//
	// The range is per host because it is a fact about that machine. The taken set
	// is not, deliberately: a block position spoken for anywhere is left alone
	// everywhere. Two hosts sharing a range would otherwise hand out the same
	// number twice, and a tunnel is -L port:localhost:port — so both would want the
	// same local port and one of them would silently not work. Across hosts whose
	// ranges do not overlap it costs nothing, since a position taken in one range
	// never appears in another.
	// Only the machines this is about to allocate on: the ones holding workspaces
	// of ours, which is exactly what `held` was built from.
	ranges := hostRanges(cfg, hostsOf(held))
	var done []Assignment
	for _, h := range held {
		if h.Block != nil || (only != "" && h.Workspace != only) {
			continue
		}
		r := rangeFor(ranges, cfg, h.Host)
		start, ok := nextFreeBlock(r, taken)
		if !ok {
			return done, fmt.Errorf("no free block left in %d-%d — widen it with: forge ports range", r.Start, r.End)
		}
		if err := callAgent(cfg.Hosts[h.Host], nil, "workspace-port-block",
			"--name", h.Workspace,
			"--port-start", strconv.Itoa(start),
			"--port-size", strconv.Itoa(r.Block),
		); err != nil {
			return done, fmt.Errorf("%s: %v", h.Workspace, err)
		}
		taken[start] = true
		done = append(done, Assignment{Workspace: h.Workspace, Block: PortBlock{Start: start, Size: r.Block}})
	}
	return done, nil
}

// SetPortRange records the span Forge allocates blocks from, and returns what
// the span ended up being. A start, end or block size of 0 means "leave that one
// alone", so a caller can move the span without restating the block size, or the
// block size without restating the span.
//
// It does not move any block that already exists — blocks are immutable, which is
// the property everything else here relies on. A new range only decides where the
// NEXT block comes from, so widening one is safe, and narrowing one below an
// existing block is refused rather than silently leaving that workspace
// publishing ports this client no longer considers its own.
func SetPortRange(start, end, block int) (config.PortRange, error) {
	cfg, err := loadConfig()
	if err != nil {
		return config.PortRange{}, err
	}
	next := cfg.PortRangeOr()
	// Each bound on its own. Moving them together looks harmless — the CLI parses a
	// span and always has both — but "0 means leave it alone" has to hold for one
	// bound as well as three, or a caller that raises only the ceiling silently
	// takes the floor to zero with it.
	if start > 0 {
		next.Start = start
	}
	if end > 0 {
		next.End = end
	}
	if block > 0 {
		next.Block = block
	}
	if len(next.Blocks()) == 0 {
		return next, fmt.Errorf("range %d-%d holds no block of %d ports", next.Start, next.End, next.Block)
	}
	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		return next, fmt.Errorf("cannot check existing blocks: %s unreachable", strings.Join(unreachable, ", "))
	}
	for _, h := range held {
		if h.Block == nil {
			continue
		}
		if h.Block.Start < next.Start || h.Block.End() > next.End {
			return next, fmt.Errorf("workspace %q holds %d-%d, which is outside %d-%d — blocks never move, so widen the range instead",
				h.Workspace, h.Block.Start, h.Block.End(), next.Start, next.End)
		}
	}
	err = updateConfig(func(c *config.Config) error {
		c.PortRange = next
		return nil
	})
	return next, err
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
// rule every other operation follows. A host may hold others; they are not ours to
// count, and their blocks (if any) belong to whoever made them.
func heldBlocks(cfg *config.Config) (held []Holder, unreachable []string) {
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
		if err := callAgent(host, &res, "workspace-list"); err != nil {
			unreachable = append(unreachable, alias)
			continue
		}
		blocks := map[string]*agentproto.PortBlock{}
		for _, w := range res.Workspaces {
			blocks[w.Name] = w.PortBlock
		}
		for _, ws := range byHost[alias] {
			held = append(held, Holder{Workspace: ws, Host: alias, Block: portBlock(blocks[ws])})
		}
	}
	return held, unreachable
}

// portBlock converts the agent's wire block into the core's own.
func portBlock(b *agentproto.PortBlock) *PortBlock {
	if b == nil {
		return nil
	}
	return &PortBlock{Start: b.Start, Size: b.Size}
}

// hostRanges is the span each host hands blocks out of.
//
// A host that keeps its own range answers with it, and that is the point: which
// of a machine's ports are free is a fact about the machine, and a second device
// has nowhere else to learn it. A host that has not been told, or is too old to
// be asked, falls back to this client's own range — which is what every host used
// until now, so nothing moves under a setup that has not been migrated.
//
// want is the hosts worth asking, and it is the caller's to decide because it
// differs: allocating for one workspace needs one machine's range, and a
// backfill needs the ranges of the machines that hold workspaces. Asking every
// registered host instead would cost a full connect timeout for each one that is
// switched off — a server nobody is allocating on holding up the ones they are.
//
// Failures are not distinguished from silence here on purpose: the callers all
// refuse to allocate against an unreachable host anyway, by name, before they
// get this far.
func hostRanges(cfg *config.Config, want map[string]bool) map[string]config.PortRange {
	fallback := cfg.PortRangeOr()
	answers := askHosts(cfg.Hosts, want,
		func(_ string, host *config.Host) (config.PortRange, error) {
			var res agentproto.PortRangeResult
			if err := callAgent(host, &res, "host-port-range"); err != nil {
				return config.PortRange{}, err
			}
			if !res.Recorded || !res.Set {
				return config.PortRange{}, errHostHasNoRange
			}
			return config.PortRange{Start: res.Start, End: res.End, Block: res.Block}, nil
		})

	out := map[string]config.PortRange{}
	for alias := range want {
		if r, ok := answers[alias]; ok {
			out[alias] = r
		} else {
			out[alias] = fallback
		}
	}
	return out
}

// hostsOf is the set of machines a list of holders sits on.
func hostsOf(held []Holder) map[string]bool {
	want := map[string]bool{}
	for _, h := range held {
		want[h.Host] = true
	}
	return want
}

// errHostHasNoRange is a host that has not been told its range, which is not a
// failure — it is every host until a client fills one in.
var errHostHasNoRange = errors.New("this host keeps no port range")

// rangeFor is one host's span, with the client's own as the answer for a host
// nobody asked about.
func rangeFor(ranges map[string]config.PortRange, cfg *config.Config, alias string) config.PortRange {
	if r, ok := ranges[alias]; ok && r.Block > 0 {
		return r
	}
	return cfg.PortRangeOr()
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

// takenBlocks is every block position that is spoken for: held by a workspace that
// exists, or reserved for one being created.
func takenBlocks(held []Holder, reserved []config.PortReservation) map[int]bool {
	taken := map[int]bool{}
	for _, h := range held {
		if h.Block != nil {
			taken[h.Block.Start] = true
		}
	}
	for _, r := range reserved {
		taken[r.Start] = true
	}
	return taken
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
func allocateBlock(cfg *config.Config, workspace, alias string) (*PortBlock, error) {
	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		return nil, fmt.Errorf("cannot reach %s — its port blocks are unknown, and allocating without them risks handing out one twice",
			strings.Join(unreachable, ", "))
	}

	r := rangeFor(hostRanges(cfg, map[string]bool{alias: true}), cfg, alias)
	var block *PortBlock
	err := updateConfig(func(c *config.Config) error {
		taken := takenBlocks(held, c.ActiveReservations(time.Now()))
		start, ok := nextFreeBlock(r, taken)
		if !ok {
			return fmt.Errorf("no free port block left in %d-%d — widen it with: forge ports range", r.Start, r.End)
		}
		c.ReservePortBlock(workspace, alias, start, time.Now())
		block = &PortBlock{Start: start, Size: r.Block}
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
	_ = updateConfig(func(c *config.Config) error {
		c.ReleasePortBlock(workspace)
		return nil
	})
}
