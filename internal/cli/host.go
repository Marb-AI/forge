package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/Marb-AI/forge/forge"
)

func hostCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge host <add|prepare|update|adopt|gh-login|list|remove>")
	}
	switch args[0] {
	case "add":
		return hostAdd(args[1:])
	case "prepare":
		return hostPrepare(args[1:])
	case "update":
		return hostUpdate(args[1:])
	case "adopt":
		return hostAdopt(args[1:])
	case "gh-login":
		return hostGhLogin(args[1:])
	case "list", "ls":
		return hostList()
	case "remove", "rm":
		return hostRemove(args[1:])
	default:
		return fail("unknown host command %q", args[0])
	}
}

// hostAdopt tells a server which of its accounts are Forge's, from what this
// client already believes.
//
// It exists because the host cannot work that out for itself: its directory
// holds every account under /home/workspaces, and until a device says otherwise
// nothing on the machine distinguishes a workspace Forge made from one somebody
// created by hand. Once it has been told, the server is the answer for every
// device — which is what a second one needs, since it has nothing of its own to
// go on.
//
// Run once per server, after `forge host update`. Running it again changes
// nothing.
func hostAdopt(args []string) int {
	if len(args) > 1 {
		return fail("usage: forge host adopt [alias]")
	}
	alias := ""
	if len(args) == 1 {
		alias = args[0]
	}
	named, err := forge.AdoptWorkspaces(alias)
	if err != nil {
		return fail("%v", err)
	}
	if len(named) == 0 {
		fmt.Println("forge: no workspaces to hand over")
		return 0
	}
	aliases := make([]string, 0, len(named))
	for a := range named {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	bad := false
	for _, a := range aliases {
		r := named[a]
		switch {
		case r.Err == nil:
			fmt.Printf("  %-14s %d workspace(s)\n", a, r.Named)
		case r.TooOld():
			// Named rather than skipped quietly: this is the one thing between that
			// server and a second device, and the fix is a command.
			fmt.Printf("  %-14s its agent is too old — run: forge host update %s\n", a, a)
			bad = true
		default:
			// Whatever else went wrong, in its own words. A server that is simply off
			// wants nothing done about it but being switched on.
			fmt.Printf("  %-14s %v\n", a, r.Err)
			bad = true
		}
	}
	if bad {
		return 1
	}
	return 0
}

// hostPrepare reads the flags of `forge host prepare` and hands the provisioning
// itself to the core, which streams its progress to stdout.
func hostPrepare(args []string) int {
	alias, rest := extractFlag(args, "alias")
	jump, jumpGiven, rest := extractFlagSet(rest, "jump")
	noFirewall := hasBoolFlag(rest, "--no-firewall")
	noHarden := hasBoolFlag(rest, "--no-ssh-harden")
	noPrune := hasBoolFlag(rest, "--no-docker-prune")
	pruneImages := hasBoolFlag(rest, "--docker-prune-images")
	pruneVolumes := hasBoolFlag(rest, "--docker-prune-volumes")
	rest = dropFlags(rest, "--no-firewall", "--no-ssh-harden", "--no-docker-prune", "--docker-prune-images", "--docker-prune-volumes")

	// The image and volume sweeps are tiers of the nightly clean-up, not standalone
	// jobs — they're injected into that script. Asking for one while declining the
	// clean-up would silently install nothing, so reject the contradiction rather
	// than no-op.
	if noPrune && pruneImages {
		return fail("--docker-prune-images is part of the nightly clean-up; drop --no-docker-prune to use it")
	}
	if noPrune && pruneVolumes {
		return fail("--docker-prune-volumes is part of the nightly clean-up; drop --no-docker-prune to use it")
	}

	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host prepare <ssh-target> --alias=<alias> [--jump=<[user@]host[:port],...>] [--no-firewall] [--no-ssh-harden] [--no-docker-prune] [--docker-prune-images] [--docker-prune-volumes]")
	}
	// Absent means "keep the route this host is already recorded with", which is
	// what re-running prepare on a host behind a bastion has to mean — see
	// forge.PrepareHost.
	var route *string
	if jumpGiven {
		route = &jump
	}
	if err := forge.PrepareHost(rest[0], alias, route, !noFirewall, !noHarden, !noPrune, pruneImages, pruneVolumes, os.Stdout); err != nil {
		return fail("%v", err)
	}
	return 0
}

func hostGhLogin(args []string) int {
	if len(args) < 1 {
		return fail("usage: forge host gh-login <alias>")
	}
	alias := args[0]
	if code := interactive(func(out io.Writer) error { return forge.GhLogin(alias, out) }); code != 0 {
		return code
	}
	fmt.Printf("\ngh authenticated for host %q.\n", alias)
	fmt.Printf("  new workspaces get it automatically; existing ones need a re-create.\n")
	return 0
}

func hostAdd(args []string) int {
	// Manual flag extraction so --alias may appear before or after the target
	// (Go's flag package stops at the first positional argument).
	alias, rest := extractFlag(args, "alias")
	jump, rest := extractFlag(rest, "jump")
	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host add <ssh-target> --alias=<alias> [--jump=<[user@]host[:port],...>]")
	}
	host, err := forge.AddHost(rest[0], alias, jump)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("added host %q -> %s\n", host.Alias, hostRoute(host.User, host.Addr, host.Port, host.Jump))
	return 0
}

// hostUpdate puts this client's agent on the hosts that are not running it. No
// argument means every registered host, as `forge ports assign` does.
func hostUpdate(args []string) int {
	var (
		ups []forge.AgentUpdate
		err error
	)
	if len(args) > 0 {
		var u forge.AgentUpdate
		u, err = forge.UpdateAgent(args[0])
		ups = []forge.AgentUpdate{u}
	} else {
		ups, err = forge.UpdateAgents()
	}
	if err != nil {
		return fail("%v", err)
	}
	if len(ups) == 0 {
		fmt.Println("no hosts registered")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	failed := 0
	for _, u := range ups {
		switch {
		case u.Err != nil:
			failed++
			fmt.Fprintf(w, "%s\t%v\n", u.Host, u.Err)
		case u.Changed:
			fmt.Fprintf(w, "%s\t%s -> %s\n", u.Host, was(u.Was), u.Now)
		default:
			fmt.Fprintf(w, "%s\talready %s\n", u.Host, u.Now)
		}
	}
	if code := flush(w); code != 0 {
		return code
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// was names what a host was running, for the one that could not say: the version
// verb arrived in v0.10.0, so silence means older than that (or no agent at all).
func was(v string) string {
	if v == "" {
		return "an agent too old to say"
	}
	return v
}

func hostList() int {
	hosts, err := forge.Hosts()
	if err != nil {
		return fail("%v", err)
	}
	if len(hosts) == 0 {
		fmt.Println("no hosts registered")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tTARGET")
	for _, h := range hosts {
		fmt.Fprintf(w, "%s\t%s\n", h.Alias, hostRoute(h.User, h.Addr, h.Port, h.Jump))
	}
	return flush(w)
}

// hostRoute is where a host is and how it is reached, for the two places that
// print it. The jump is shown as it was typed: it is what would have to be
// corrected. Taken apart rather than as a host, because naming that type is
// reaching past the core — see adapter_test.go.
func hostRoute(user, addr string, port int, jump string) string {
	route := fmt.Sprintf("%s@%s:%d", user, addr, port)
	if jump != "" {
		route += " via " + jump
	}
	return route
}

func hostRemove(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge host remove <alias>")
	}
	if err := forge.RemoveHost(args[0]); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("removed host %q\n", args[0])
	return 0
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		return fail("%v", err)
	}
	return 0
}
