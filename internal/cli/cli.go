// Package cli is the laptop-side command surface: a small hand-written
// dispatcher (no external CLI framework) over the operations in package forge.
// See the README for the command tree.
//
// It is an adapter and nothing more. Every command here reads argv, calls one
// operation on the core, and formats what comes back — no command talks to a
// server, reads the config, or starts a process of its own. The browser UI is
// the same shape over HTTP and JSON, and that symmetry is the point: whichever
// front end you use, you get the same Forge, because there is only one.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/Marb-AI/forge/forge"
)

const usage = `forge — remote Claude Code workspace manager

Hosts:
  forge host prepare <ssh-target> --alias=<alias>  provision a bare server + register it
                                                  [--no-firewall] [--no-ssh-harden] [--no-docker-prune]
                                                  [--docker-prune-images] [--docker-prune-volumes]
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

UI:
  forge ui [start]                               start the local browser UI + open it
  forge ui stop
  forge ui status
  forge ui port <port>                           set the UI's localhost port

Ports:
  forge ports                                    which workspace owns which block
  forge ports range [<start>-<end>] [--block=N]  the span Forge allocates blocks from
  forge ports assign [name]                      give a block to workspaces without one

Info:
  forge show ports [host]                        listening + forwarded ports
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
	case "ports":
		return portsCmd(args[1:])
	case "spawn":
		return spawnCmd(args[1:])
	case "ui":
		return uiCmd(args[1:])
	case forge.RunSupervisorArg: // hidden: the detached daemon re-execs itself with this
		return runSupervisor()
	case forge.RunUIArg: // hidden: the detached UI daemon re-execs itself with this
		return runUI()
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

// dropFlags removes the given boolean flags from args.
func dropFlags(args []string, flags ...string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if !contains(flags, a) {
			out = append(out, a)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
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
