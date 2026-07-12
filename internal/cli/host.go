package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
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

// hostGhLogin authenticates gh once per host, into the host's own gh config
// directory rather than the admin's home. `workspace create` then copies that
// credential into each new workspace, so you log in once per server instead of
// once per workspace — the same shape as the host's git identity.
//
// The login itself is interactive (a browser code, or a token on stdin), so it
// cannot happen during `prepare`; it gets its own command and a TTY.
func hostGhLogin(args []string) int {
	if len(args) < 1 {
		return fail("usage: forge host gh-login <alias>")
	}
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	host := cfg.Hosts[args[0]]
	if host == nil {
		return fail("no such host %q (see: forge host list)", args[0])
	}

	// GH_CONFIG_DIR puts hosts.yml under /etc/forge/gh instead of ~/.config/gh.
	// The file holds a token, so it stays root-only; the agent copies it in as
	// root at create time.
	remote := "install -d -m 0755 " + hostGhDir +
		" && GH_CONFIG_DIR=" + hostGhDir + " gh auth login" +
		" && chmod 0600 " + hostGhDir + "/hosts.yml"
	if host.User != "root" {
		remote = "sudo sh -c '" + remote + "'"
	}

	fmt.Printf("logging gh in on %s (interactive)…\n", args[0])
	if code := runInteractive(sshx.Target{User: host.User, Addr: host.Addr, Port: host.Port}.TTYArgs(remote)); code != 0 {
		return code
	}
	fmt.Printf("\ngh authenticated for host %q.\n", args[0])
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
	if err := removeHost(args[0]); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("removed host %q\n", args[0])
	return 0
}

// removeHost forgets a server locally. It does NOT touch the machine: the server
// keeps running, and so do its workspaces — Forge just stops knowing about them.
// Shared by `forge host remove` and the UI's settings panel.
func removeHost(alias string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Hosts[alias]; !ok {
		return fmt.Errorf("no such host %q", alias)
	}
	delete(cfg.Hosts, alias)
	delete(cfg.Ports, alias)
	for ws, host := range cfg.Workspaces {
		if host == alias {
			delete(cfg.Workspaces, ws)
		}
	}
	return cfg.Save()
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		return fail("%v", err)
	}
	return 0
}
