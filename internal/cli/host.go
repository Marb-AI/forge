package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/Marb-AI/forge/internal/config"
)

func hostCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge host <add|list|remove>")
	}
	switch args[0] {
	case "add":
		return hostAdd(args[1:])
	case "list", "ls":
		return hostList()
	case "remove", "rm":
		return hostRemove(args[1:])
	default:
		return fail("unknown host command %q", args[0])
	}
}

func hostAdd(args []string) int {
	// Manual flag extraction so --alias may appear before or after the target
	// (Go's flag package stops at the first positional argument).
	alias, rest := extractFlag(args, "alias")
	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host add <ssh-target> --alias=<alias>")
	}
	target := rest[0]

	user, addr, port, err := config.ParseSSHTarget(target)
	if err != nil {
		return fail("%v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	if _, exists := cfg.Hosts[alias]; exists {
		return fail("host %q already exists", alias)
	}
	cfg.Hosts[alias] = &config.Host{Alias: alias, User: user, Addr: addr, Port: port}
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("added host %q -> %s@%s:%d\n", alias, user, addr, port)
	return 0
}

func hostList() int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Println("no hosts registered")
		return 0
	}
	aliases := make([]string, 0, len(cfg.Hosts))
	for a := range cfg.Hosts {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tTARGET")
	for _, a := range aliases {
		h := cfg.Hosts[a]
		fmt.Fprintf(w, "%s\t%s@%s:%d\n", h.Alias, h.User, h.Addr, h.Port)
	}
	return flush(w)
}

func hostRemove(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge host remove <alias>")
	}
	alias := args[0]
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	if _, ok := cfg.Hosts[alias]; !ok {
		return fail("no such host %q", alias)
	}
	delete(cfg.Hosts, alias)
	delete(cfg.Ports, alias)
	for ws, host := range cfg.Workspaces {
		if host == alias {
			delete(cfg.Workspaces, ws)
		}
	}
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("removed host %q\n", alias)
	return 0
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		return fail("%v", err)
	}
	return 0
}
