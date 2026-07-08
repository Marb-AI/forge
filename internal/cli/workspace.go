package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

func workspaceCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge workspace <create|delete|list> | <name> <ssh|claude|expose>")
	}
	switch args[0] {
	case "create":
		return workspaceCreate(args[1:])
	case "delete", "rm":
		return workspaceDelete(args[1:])
	case "list", "ls":
		return workspaceList()
	default:
		// `forge workspace <name> <action> ...`
		name := args[0]
		if len(args) < 2 {
			return fail("usage: forge workspace %s <ssh|claude|expose>", name)
		}
		return workspaceAction(name, args[1], args[2:])
	}
}

func workspaceCreate(args []string) int {
	if len(args) < 2 {
		return fail("usage: forge workspace create <name> <host-alias>")
	}
	name, alias := args[0], args[1]

	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	host := cfg.Hosts[alias]
	if host == nil {
		return fail("no such host %q (see: forge host list)", alias)
	}

	pubkey, err := findPublicKey()
	if err != nil {
		return fail("%v", err)
	}
	enc := base64.StdEncoding.EncodeToString(pubkey)

	var res agentproto.CreateResult
	if err := callAgent(host, &res, "workspace-create", "--name", name, "--pubkey", enc); err != nil {
		return fail("%v", err)
	}

	cfg.AddWorkspace(name, alias)
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("created workspace %q on %s\n", name, alias)
	fmt.Printf("  next: forge workspace %s claude\n", name)
	return 0
}

func workspaceDelete(args []string) int {
	if len(args) < 1 {
		return fail("usage: forge workspace delete <name>")
	}
	name := args[0]
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	host := cfg.HostFor(name)
	if host == nil {
		return fail("unknown workspace %q — not created by this client", name)
	}
	if err := callAgent(host, nil, "workspace-delete", "--name", name); err != nil {
		return fail("%v", err)
	}
	cfg.RemoveWorkspace(name)
	if err := cfg.Save(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("deleted workspace %q\n", name)
	return 0
}

func workspaceList() int {
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
	fmt.Fprintln(w, "NAME\tHOST\tSTATUS")
	for _, alias := range aliases {
		var res agentproto.ListResult
		if err := callAgent(cfg.Hosts[alias], &res, "workspace-list"); err != nil {
			fmt.Fprintf(w, "-\t%s\t(unreachable)\n", alias)
			continue
		}
		sort.Slice(res.Workspaces, func(i, j int) bool {
			return res.Workspaces[i].Name < res.Workspaces[j].Name
		})
		for _, ws := range res.Workspaces {
			fmt.Fprintf(w, "%s\t%s\t%s\n", ws.Name, alias, ws.Status)
		}
	}
	return flush(w)
}

// workspaceAction handles `forge workspace <name> <ssh|claude|expose>`.
func workspaceAction(name, action string, rest []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	host := cfg.HostFor(name)
	if host == nil {
		return fail("unknown workspace %q — not created by this client", name)
	}
	target := sshx.WorkspaceTarget(host, name)

	switch action {
	case "ssh":
		return runInteractive(target.TTYArgs())
	case "claude":
		return workspaceClaude(target, rest)
	case "expose":
		return workspaceExpose(target, rest)
	default:
		return fail("unknown action %q (want ssh|claude|expose)", action)
	}
}

func workspaceClaude(target sshx.Target, rest []string) int {
	session := agentproto.TmuxSession
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "attach":
		// attach-or-create in one command; survives disconnect via tmux.
		return runInteractive(target.TTYArgs("tmux", "new", "-A", "-s", session, "claude"))
	case "renew":
		// kill the existing session (reset context) then start fresh and attach.
		remote := fmt.Sprintf("tmux kill-session -t %s 2>/dev/null; tmux new -A -s %s claude", session, session)
		return runInteractive(target.TTYArgs(remote))
	case "stop":
		if err := runCapture(target.Args("tmux", "kill-session", "-t", session)); err != nil {
			return fail("stop: %v (session may not be running)", err)
		}
		fmt.Println("claude session stopped")
		return 0
	default:
		return fail("usage: forge workspace <name> claude [renew|stop]")
	}
}

func workspaceExpose(target sshx.Target, rest []string) int {
	if len(rest) < 1 {
		return fail("usage: forge workspace <name> expose <port>")
	}
	port, err := strconv.Atoi(rest[0])
	if err != nil {
		return fail("invalid port %q", rest[0])
	}
	fmt.Printf("exposing localhost:%d  (Ctrl-C to stop)\n", port)
	// Foreground, blocks until Ctrl-C. For always-on tunnels use forwarding.
	return runInteractive(target.LocalForwardArgs(port, port))
}

// findPublicKey returns the client's SSH public key to install into the
// workspace user's authorized_keys. FORGE_PUBKEY overrides the search.
func findPublicKey() ([]byte, error) {
	if p := os.Getenv("FORGE_PUBKEY"); p != "" {
		return os.ReadFile(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", name)
		if data, err := os.ReadFile(p); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("no SSH public key found in ~/.ssh (set FORGE_PUBKEY to override)")
}

func runInteractive(args []string) int {
	if err := sshx.RunInteractive(args...); err != nil {
		// Interactive exit codes (e.g. Ctrl-C) are normal; don't shout.
		return 1
	}
	return 0
}

func runCapture(args []string) error {
	_, err := sshx.Capture(args...)
	return err
}
