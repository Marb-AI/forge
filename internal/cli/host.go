package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/Marb-AI/forge/forge"
)

func hostCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge host <add|prepare|gh-login|list|remove>")
	}
	switch args[0] {
	case "add":
		return hostAdd(args[1:])
	case "prepare":
		return hostPrepare(args[1:])
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

// hostPrepare reads the flags of `forge host prepare` and hands the provisioning
// itself to the core, which streams its progress to stdout.
func hostPrepare(args []string) int {
	alias, rest := extractFlag(args, "alias")
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
		return fail("usage: forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden] [--no-docker-prune] [--docker-prune-images] [--docker-prune-volumes]")
	}
	if err := forge.PrepareHost(rest[0], alias, !noFirewall, !noHarden, !noPrune, pruneImages, pruneVolumes, os.Stdout); err != nil {
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
	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host add <ssh-target> --alias=<alias>")
	}
	host, err := forge.AddHost(rest[0], alias)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("added host %q -> %s@%s:%d\n", host.Alias, host.User, host.Addr, host.Port)
	return 0
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
		fmt.Fprintf(w, "%s\t%s@%s:%d\n", h.Alias, h.User, h.Addr, h.Port)
	}
	return flush(w)
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
