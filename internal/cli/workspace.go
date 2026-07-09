package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

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
		args := target.TTYArgs()
		// Forward the local SSH agent by default, so git operations in the
		// workspace use your keys with no credential stored on the server.
		// Opt out with --no-agent.
		if !hasBoolFlag(rest, "--no-agent") {
			args = append([]string{"-A"}, args...)
		}
		return runInteractive(args)
	case "claude":
		return workspaceClaude(target, rest)
	case "expose":
		return workspaceExpose(target, rest)
	default:
		return fail("unknown action %q (want ssh|claude|expose)", action)
	}
}

// sourceEnv sources the workspace environment file before launching, so the
// Claude/tmux session gets COMPOSE_PROJECT_NAME et al. even though it is not an
// interactive shell that would read .bashrc. `set -a` exports everything sourced.
const sourceEnv = `set -a; [ -f "$HOME/.forge/env" ] && . "$HOME/.forge/env"; set +a; `

// workspaceClaude launches plain `claude` in tmux. tmux gives the persistence:
// detach (Ctrl-b d) keeps the session to reattach later; /exit or Ctrl-C ends
// Claude, the command finishes, the tmux session is gone, and the next launch is
// a clean new session — a killed session stays killed, never offered for resume.
//
// Remote Control is intentionally NOT auto-started here (its resume-the-last-
// session behaviour breaks that guarantee). To surface a session in the Claude
// app, run `/remote-control` inside it — it's named after the workspace via
// CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX in the env.
func workspaceClaude(target sshx.Target, rest []string) int {
	session := agentproto.TmuxSession
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "attach":
		// attach-or-create in one command; survives disconnect via tmux.
		remote := sourceEnv + fmt.Sprintf("tmux new -A -s %s claude", session)
		return runInteractive(target.TTYArgs(remote))
	case "renew":
		// kill the existing session (reset context) then start fresh and attach.
		remote := fmt.Sprintf("tmux kill-session -t %s 2>/dev/null; ", session) +
			sourceEnv + fmt.Sprintf("tmux new -A -s %s claude", session)
		return runInteractive(target.TTYArgs(remote))
	case "stop":
		if err := runCapture(target.Args("tmux", "kill-session", "-t", session)); err != nil {
			return fail("stop: %v (session may not be running)", err)
		}
		fmt.Println("claude session stopped")
		return 0
	case "checkpoint":
		return workspaceCheckpoint(target, session)
	default:
		return fail("usage: forge workspace <name> claude [renew|stop|checkpoint]")
	}
}

// checkpointMarker is the standalone line Claude is asked to print when the
// handoff is written. It is matched only as a whole trimmed line, so its mention
// inside the (echoed) prompt — mid-sentence — doesn't count.
const checkpointMarker = "FORGE_CHECKPOINT_SAVED"

// workspaceCheckpoint asks the running Claude session to write a handoff to its
// memory, waits for it to finish, then restarts the session so it continues from
// memory with a fresh context window. Run it from another terminal while the
// session is idle.
func workspaceCheckpoint(target sshx.Target, session string) int {
	if err := runCapture(target.Args("tmux", "has-session", "-t", session)); err != nil {
		return fail("no running claude session to checkpoint (start one with: forge workspace <name> claude)")
	}
	// Safe gate: only proceed when the pane is stable (no task streaming output).
	if !claudeIdle(target, session) {
		return fail("Claude looks busy — run checkpoint when it's idle (nothing running)")
	}

	// The marker is embedded mid-sentence (words before and after) so its echo in
	// the typed prompt can't wrap into a standalone marker line and false-positive;
	// Claude's own output prints it alone on a line, which is what we match.
	prompt := "Write a concise handoff to your memory right now — what we're working on, " +
		"the current state, and the exact next steps — so a brand-new session can continue " +
		"seamlessly. Do not ask me anything; just do it. When the memory is fully written, " +
		"print the token " + checkpointMarker + " alone on its own line and then stop."
	fmt.Println("→ asking Claude to write a handoff to memory…")
	if err := sendText(target, session, prompt); err != nil {
		return fail("send prompt: %v", err)
	}

	if !waitForCheckpoint(target, session, 3*time.Minute) {
		return fail("Claude didn't confirm the handoff in time — left the session running, nothing killed")
	}

	fmt.Println("→ handoff saved; restarting the session from memory…")
	_ = runCapture(target.Args("tmux", "kill-session", "-t", session))
	launch := sourceEnv + fmt.Sprintf("tmux new -d -s %s 'claude \"continue from memory\"'", session)
	if err := runCapture(target.Args(launch)); err != nil {
		return fail("restart: %v (start it manually with: forge workspace <name> claude)", err)
	}
	fmt.Println("done — fresh session running from memory. Reattach with: forge workspace <name> claude")
	return 0
}

// sendText types text into the tmux session and presses Enter. The text is piped
// through a tmux paste buffer via stdin — never as a shell argument — so quotes,
// apostrophes and other metacharacters in the prompt can't break remote parsing.
func sendText(target sshx.Target, session, text string) error {
	const buf = "forgecp"
	if err := sshx.RunWithInput(strings.NewReader(text),
		target.Args("tmux", "load-buffer", "-b", buf, "-")...); err != nil {
		return err
	}
	if _, err := sshx.Capture(target.Args("tmux", "paste-buffer", "-d", "-b", buf, "-t", session)...); err != nil {
		return err
	}
	_, err := sshx.Capture(target.Args("tmux", "send-keys", "-t", session, "Enter")...)
	return err
}

// capturePane returns the visible pane text of the tmux session.
func capturePane(target sshx.Target, session string) (string, bool) {
	out, err := sshx.Capture(target.Args("tmux", "capture-pane", "-t", session, "-p")...)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// claudeIdle reports whether the pane is stable across a short window — i.e. no
// task is streaming output. Version-independent (no reliance on TUI wording).
func claudeIdle(target sshx.Target, session string) bool {
	a, ok := capturePane(target, session)
	if !ok {
		return false
	}
	time.Sleep(2 * time.Second)
	b, ok := capturePane(target, session)
	return ok && a == b
}

// waitForCheckpoint waits until Claude prints the marker on its own line (and has
// actually produced a response, not just echoed the prompt), then a moment more
// so the memory write settles.
func waitForCheckpoint(target sshx.Target, session string, timeout time.Duration) bool {
	time.Sleep(3 * time.Second) // let the prompt register and Claude start
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pane, ok := capturePane(target, session); ok && hasMarkerLine(pane, checkpointMarker) {
			time.Sleep(2 * time.Second)
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// hasMarkerLine reports whether any whole (trimmed) line of s equals the marker.
func hasMarkerLine(s, marker string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
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
