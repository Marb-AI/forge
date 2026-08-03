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
                                                  [--jump=<[user@]host[:port],...>] reach it through these servers
                                                  (kept as recorded when re-run without it; --jump= clears it)
                                                  [--no-firewall] [--no-ssh-harden] [--no-docker-prune]
                                                  [--docker-prune-images] [--docker-prune-volumes]
  forge host add <ssh-target> --alias=<alias>   register an already-prepared server
                                                [--jump=<[user@]host[:port],...>]
  forge host update [alias]                     put this client's agent on the host(s)
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
  forge ui status [-q]                           -q: nothing printed, exit 0 when it runs
  forge ui port <port>                           set the UI's localhost port

Ports:
  forge ports                                    which workspace owns which block
  forge ports range [<start>-<end>] [--block=N]  the span Forge allocates blocks from
  forge ports assign [name]                      give a block to workspaces without one

Info:
  forge show ports [host]                        listening + forwarded ports
  forge version [host-alias]                     which build this is (and the agent's, on a host)
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
	case "version", "--version", "-v":
		return versionCmd(args[1:])
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

// versionCmd prints this client's build, and — when a host is named — the build
// that installed the agent on it. Two answers rather than one because they can
// differ: the agent is uploaded by `host prepare`, so a server keeps whatever
// the client that last prepared it carried until someone prepares it again.
func versionCmd(args []string) int {
	fmt.Printf("forge %s\n", forge.Version())
	if len(args) == 0 {
		return 0
	}
	agent, err := forge.AgentVersion(args[0])
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("agent on %s %s\n", args[0], agent)
	return 0
}

// extractFlag pulls a --name=value / --name value (or single-dash) flag out of
// args wherever it appears, returning its value and the remaining positional
// args. Unlike the stdlib flag package this tolerates flags placed after
// positionals, so `host add root@host --alias=x` works.
func extractFlag(args []string, name string) (value string, rest []string) {
	value, _, rest = extractFlagSet(args, name)
	return value, rest
}

// extractFlagSet is the same, for a flag whose absence means something other
// than its empty value: `--jump=` clears a host's route, while no --jump at all
// keeps the one already recorded (see forge.PrepareHost).
func extractFlagSet(args []string, name string) (value string, given bool, rest []string) {
	rest = make([]string, 0, len(args))
	long, short := "--"+name, "-"+name
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == long || a == short:
			given = true
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
		case strings.HasPrefix(a, long+"="):
			value, given = a[len(long)+1:], true
		case strings.HasPrefix(a, short+"="):
			value, given = a[len(short)+1:], true
		default:
			rest = append(rest, a)
		}
	}
	return value, given, rest
}
