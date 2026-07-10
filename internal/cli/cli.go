// Package cli is the laptop-side command surface. It is a small hand-written
// dispatcher (no external CLI framework); see the README for the command tree.
package cli

import (
	"fmt"
	"os"
	"strings"
)

const usage = `forge — remote Claude Code workspace manager

Hosts:
  forge host prepare <ssh-target> --alias=<alias>  provision a bare server + register it
  forge host add <ssh-target> --alias=<alias>   register an already-prepared server
  forge host gh-login <alias>                   authenticate gh once for the whole host
  forge host list
  forge host remove <alias>

Workspaces:
  forge workspace create <name> <host-alias>
  forge workspace delete <name>
  forge workspace list

  forge workspace <name> ssh [--no-agent]        shell as the workspace user (SSH agent forwarded by default)
  forge workspace <name> claude [renew|stop]     persistent Claude session (tmux)
  forge workspace <name> claude checkpoint       save a handoff to memory, then restart from it (fresh context)
  forge workspace <name> expose <port>           one-off ssh -L, foreground

Forwarding:
  forge forwarding start [name]                  scan docker ports, save, (re)spawn
  forge forwarding stop
  forge forwarding status
  forge spawn                                     ensure the tunnel supervisor is up

Info:
  forge show ports [host]                        listening + forwarded ports (paste to Claude)
`

// Main is the CLI entrypoint. It returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2
	}
	switch args[0] {
	case "host":
		return hostCmd(args[1:])
	case "workspace", "ws":
		return workspaceCmd(args[1:])
	case "forwarding", "fwd":
		return forwardingCmd(args[1:])
	case "show":
		return showCmd(args[1:])
	case "spawn":
		return spawnCmd(args[1:])
	case runSupervisorArg: // hidden: the detached daemon re-execs itself with this
		return runSupervisor(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "forge: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// fail prints an error to stderr and returns exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "forge: "+format+"\n", a...)
	return 1
}

// hasBoolFlag reports whether any of names appears verbatim in args.
func hasBoolFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// extractFlag pulls a --name=value / --name value (or single-dash) flag out of
// args wherever it appears, returning its value and the remaining positional
// args. Unlike the stdlib flag package this tolerates flags placed after
// positionals, so `host add root@host --alias=x` works.
func extractFlag(args []string, name string) (value string, rest []string) {
	rest = make([]string, 0, len(args))
	long, short := "--"+name, "-"+name
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == long || a == short:
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
		case strings.HasPrefix(a, long+"="):
			value = a[len(long)+1:]
		case strings.HasPrefix(a, short+"="):
			value = a[len(short)+1:]
		default:
			rest = append(rest, a)
		}
	}
	return value, rest
}
