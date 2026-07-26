package cli

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/forge"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/clip"
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
	block, err := forge.CreateWorkspace(name, alias)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("created workspace %q on %s\n", name, alias)
	if block != nil {
		fmt.Printf("  host ports %d-%d — Claude knows; you never paste them\n", block.Start, block.End())
	}
	fmt.Printf("  next: forge workspace %s claude\n", name)
	return 0
}

func workspaceDelete(args []string) int {
	if len(args) < 1 {
		return fail("usage: forge workspace delete <name>")
	}
	name := args[0]
	if err := forge.DeleteWorkspace(name); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("deleted workspace %q\n", name)
	return 0
}

func workspaceList() int {
	list, err := forge.ListWorkspaces()
	if err != nil {
		return fail("%v", err)
	}
	if len(list) == 0 {
		fmt.Println("no workspaces (create one: forge workspace create <name> <host>)")
		return 0
	}

	// CLAUDE, not STATUS: what is reported is the state of the Claude session, not
	// of the workspace, which exists until you delete it.
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tCLAUDE")
	for _, ws := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\n", ws.Name, ws.Host, ws.Status)
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
		args := target.TTYArgs()
		// Forward the local SSH agent by default, so git operations in the
		// workspace use your keys with no credential stored on the server.
		// Opt out with --no-agent.
		if !hasBoolFlag(rest, "--no-agent") {
			args = append([]string{"-A"}, args...)
		}
		return runInteractive(args)
	case "claude":
		return workspaceClaude(name, target, rest)
	case "expose":
		return workspaceExpose(target, rest)
	default:
		return fail("unknown action %q (want ssh|claude|expose)", action)
	}
}

// workspaceClaude launches plain `claude` in tmux. tmux gives the persistence:
// detach (Ctrl-b d) keeps the session to reattach later; /exit or Ctrl-C ends
// Claude, the command finishes, the tmux session is gone, and the next launch is
// a clean new session — a killed session stays killed, never offered for resume.
//
// Remote Control is intentionally NOT auto-started here (its resume-the-last-
// session behaviour breaks that guarantee). To surface a session in the Claude
// app, run `/remote-control` inside it — it's named after the workspace via
// CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX in the env.
func workspaceClaude(name string, target sshx.Target, rest []string) int {
	session := agentproto.TmuxSession
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "attach":
		// attach-or-create in one command; survives disconnect via tmux.
		return runInteractive(target.TTYArgs(agentproto.AttachClaude))
	case "renew":
		// kill the existing session (reset context) then start fresh and attach.
		remote := agentproto.KillClaude + "; " + agentproto.AttachClaude
		return runInteractive(target.TTYArgs(remote))
	case "stop":
		if err := runCapture(target.Args("tmux", "kill-session", "-t", session)); err != nil {
			return fail("stop: %v (session may not be running)", err)
		}
		fmt.Println("claude session stopped")
		return 0
	case "checkpoint":
		return workspaceCheckpoint(name)
	default:
		return fail("usage: forge workspace <name> claude [renew|stop|checkpoint]")
	}
}

// workspaceCheckpoint saves a handoff to the workspace's memory and restarts its
// Claude session from it, printing the core's progress as it arrives. Run it from
// another terminal while the session is idle.
func workspaceCheckpoint(name string) int {
	if err := forge.Checkpoint(name, os.Stdout); err != nil {
		return fail("%v", err)
	}
	fmt.Println("done — fresh session running from memory. Reattach with: forge workspace <name> claude")
	return 0
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

// runInteractive runs an interactive ssh session with its output passing through
// the clipboard filter, so text copied inside the session (Claude's "press c" on
// the login URL, a tmux yank) reaches the clipboard on *this* machine whatever
// terminal it is being run in — Terminal.app has never supported OSC 52, and Warp
// now denies it by default. See internal/clip.
func runInteractive(args []string) int {
	f := clip.NewFilter(os.Stdout)
	err := sshx.RunInteractiveTo(f, args...)
	// Emit anything held back mid-escape when the session ended. A session that
	// ended badly has already said so — but if ssh was happy and the flush is not,
	// then the last thing the session drew never reached the screen, and only this
	// return value is left to say so.
	if ferr := f.Flush(); ferr != nil && err == nil {
		return fail("terminal output: %v", ferr)
	}
	if err != nil {
		// Interactive exit codes (e.g. Ctrl-C) are normal; don't shout.
		return 1
	}
	return 0
}

func runCapture(args []string) error {
	_, err := sshx.Capture(args...)
	return err
}
