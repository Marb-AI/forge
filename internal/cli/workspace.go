package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/Marb-AI/forge/forge"
	"github.com/Marb-AI/forge/internal/clip"
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
	switch action {
	case "ssh":
		return interactive(func(out io.Writer) error { return forge.Shell(name, out) })
	case "claude":
		return workspaceClaude(name, rest)
	case "expose":
		return workspaceExpose(name, rest)
	default:
		return fail("unknown action %q (want ssh|claude|expose)", action)
	}
}

func workspaceClaude(name string, rest []string) int {
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "attach":
		return interactive(func(out io.Writer) error { return forge.AttachClaude(name, false, out) })
	case "renew":
		return interactive(func(out io.Writer) error { return forge.AttachClaude(name, true, out) })
	case "stop":
		if err := forge.KillClaude(name); err != nil {
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

func workspaceExpose(name string, rest []string) int {
	if len(rest) < 1 {
		return fail("usage: forge workspace <name> expose <port>")
	}
	port, err := strconv.Atoi(rest[0])
	if err != nil {
		return fail("invalid port %q", rest[0])
	}
	fmt.Printf("exposing localhost:%d  (Ctrl-C to stop)\n", port)
	return interactive(func(out io.Writer) error { return forge.ExposePort(name, port, out) })
}

// interactive runs one of the core's terminal sessions with its output passing
// through the clipboard filter, so text copied inside the session (Claude's
// "press c" on the login URL, a tmux yank) reaches the clipboard on *this*
// machine whatever terminal it is being run in — Terminal.app has never
// supported OSC 52, and Warp now denies it by default. See internal/clip.
//
// The filter is the CLI's own business: it exists because of the terminal this
// front end is attached to, and a front end without one has nothing to do with
// it. Which is why the core takes a writer and asks no questions about it.
func interactive(run func(io.Writer) error) int {
	f := clip.NewFilter(os.Stdout)
	err := run(f)
	// Emit anything held back mid-escape when the session ended. A session that
	// ended badly has already said so — but if the session was happy and the flush
	// is not, then the last thing it drew never reached the screen, and only this
	// return value is left to say so.
	if ferr := f.Flush(); ferr != nil && err == nil {
		return fail("terminal output: %v", ferr)
	}
	var exit *forge.ExitError
	if errors.As(err, &exit) {
		// The session ran and ended non-zero — a Ctrl-C, a remote exit. Normal, and
		// whatever happened was on screen; don't shout over it.
		return 1
	}
	if err != nil {
		return fail("%v", err)
	}
	return 0
}
