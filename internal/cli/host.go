package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/forge"
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

// hostPrepare reads the flags of `forge host prepare` and hands the provisioning
// itself to the core, which streams its progress to stdout.
func hostPrepare(args []string) int {
	alias, rest := extractFlag(args, "alias")
	noFirewall := hasBoolFlag(rest, "--no-firewall")
	noHarden := hasBoolFlag(rest, "--no-ssh-harden")
	noPrune := hasBoolFlag(rest, "--no-docker-prune")
	pruneImages := hasBoolFlag(rest, "--docker-prune-images")
	rest = dropFlags(rest, "--no-firewall", "--no-ssh-harden", "--no-docker-prune", "--docker-prune-images")

	// The image sweep is a tier of the nightly clean-up, not a standalone job — it's
	// injected into that script. Asking for it while declining the clean-up would
	// silently install nothing, so reject the contradiction rather than no-op.
	if noPrune && pruneImages {
		return fail("--docker-prune-images is part of the nightly clean-up; drop --no-docker-prune to use it")
	}

	if len(rest) < 1 || alias == "" {
		return fail("usage: forge host prepare <ssh-target> --alias=<alias> [--no-firewall] [--no-ssh-harden] [--no-docker-prune] [--docker-prune-images]")
	}
	if err := forge.PrepareHost(rest[0], alias, !noFirewall, !noHarden, !noPrune, pruneImages, os.Stdout); err != nil {
		return fail("%v", err)
	}
	return 0
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
	remote := "install -d -m 0755 " + forge.HostGhDir +
		" && GH_CONFIG_DIR=" + forge.HostGhDir + " gh auth login" +
		" && chmod 0600 " + forge.HostGhDir + "/hosts.yml"
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

	// The "already exists" check belongs inside the update, not before it: checked
	// against a copy loaded earlier, two adds of the same alias would both pass it
	// and the second would overwrite the first.
	if err := config.Update(func(c *config.Config) error {
		if _, exists := c.Hosts[alias]; exists {
			return fmt.Errorf("host %q already exists", alias)
		}
		c.Hosts[alias] = &config.Host{Alias: alias, User: user, Addr: addr, Port: port}
		return nil
	}); err != nil {
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
