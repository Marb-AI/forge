package forge

import (
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

// HeldBlocks reports which block each of this client's workspaces holds, plus
// the aliases of the hosts that could not be asked — whose blocks are therefore
// unknown, which is not the same as absent.
func HeldBlocks() ([]Holder, []string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	held, unreachable := heldBlocks(cfg)
	return held, unreachable, nil
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
	cfg, err := config.Load()
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
	r := cfg.PortRangeOr()
	var done []Assignment
	for _, h := range held {
		if h.Block != nil || (only != "" && h.Workspace != only) {
			continue
		}
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

// SetPortRange records the span Forge allocates blocks from.
//
// It does not move any block that already exists — blocks are immutable, which is
// the property everything else here relies on. A new range only decides where the
// NEXT block comes from, so widening one is safe, and narrowing one below an
// existing block is refused rather than silently leaving that workspace
// publishing ports this client no longer considers its own.
func SetPortRange(next config.PortRange) error {
	if len(next.Blocks()) == 0 {
		return fmt.Errorf("range %d-%d holds no block of %d ports", next.Start, next.End, next.Block)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	held, unreachable := heldBlocks(cfg)
	if len(unreachable) > 0 {
		return fmt.Errorf("cannot check existing blocks: %s unreachable", strings.Join(unreachable, ", "))
	}
	for _, h := range held {
		if h.Block == nil {
			continue
		}
		if h.Block.Start < next.Start || h.Block.End() > next.End {
			return fmt.Errorf("workspace %q holds %d-%d, which is outside %d-%d — blocks never move, so widen the range instead",
				h.Workspace, h.Block.Start, h.Block.End(), next.Start, next.End)
		}
	}
	return config.Update(func(c *config.Config) error {
		c.PortRange = next
		return nil
	})
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

	r := cfg.PortRangeOr()
	var block *PortBlock
	err := config.Update(func(c *config.Config) error {
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
	_ = config.Update(func(c *config.Config) error {
		c.ReleasePortBlock(workspace)
		return nil
	})
}
