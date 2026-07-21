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
	"unicode"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/clip"
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
	if err := createWorkspace(name, alias); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("created workspace %q on %s\n", name, alias)
	fmt.Printf("  next: forge workspace %s claude\n", name)
	return 0
}

// createWorkspace provisions a workspace on a registered host and records it
// locally. Shared by `forge workspace create` and the browser UI's wizard, so
// both take exactly the same path.
func createWorkspace(name, alias string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host := cfg.Hosts[alias]
	if host == nil {
		return fmt.Errorf("no such host %q (see: forge host list)", alias)
	}

	pubkey, err := findPublicKey()
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(pubkey)

	var res agentproto.CreateResult
	if err := callAgent(host, &res, "workspace-create", "--name", name, "--pubkey", enc); err != nil {
		return err
	}

	cfg.AddWorkspace(name, alias)
	return cfg.Save()
}

func workspaceDelete(args []string) int {
	if len(args) < 1 {
		return fail("usage: forge workspace delete <name>")
	}
	name := args[0]
	if err := deleteWorkspace(name); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("deleted workspace %q\n", name)
	return 0
}

// deleteWorkspace destroys a workspace on its host and forgets it locally.
// This is irreversible: the agent runs `userdel -r`, so the workspace's Linux
// user and its entire home — every file in it — are gone. Shared by
// `forge workspace delete` and the UI's settings panel.
func deleteWorkspace(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host := cfg.HostFor(name)
	if host == nil {
		return fmt.Errorf("unknown workspace %q — not created by this client", name)
	}
	if err := callAgent(host, nil, "workspace-delete", "--name", name); err != nil {
		return err
	}
	cfg.RemoveWorkspace(name)
	return cfg.Save()
}

// WorkspaceStatus is one workspace of ours, and the state of the Claude session in
// it. The status is the session's — a workspace is a Linux user and a home
// directory, and cannot itself be "stopped".
type WorkspaceStatus struct {
	Name   string
	Host   string
	Status string
}

// listWorkspaces returns the workspaces THIS CLIENT created, with the state of the
// Claude session in each.
//
// The list comes from our config, not from the host. The host's own list is every
// directory under /home/workspaces — including ones Forge never made, belonging to
// someone else or created by hand. Those are not ours: every command here refuses
// to touch a workspace that isn't in the config ("not created by this client"), so
// listing them only offers what we will then decline to do.
//
// The host is still asked, but only for what the config cannot know: whether a
// Claude session is running. That answer costs an SSH round trip, which is why the
// name and host — the parts we already have — are never made to wait for it.
func listWorkspaces() ([]WorkspaceStatus, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	// Ask only the hosts we actually have workspaces on, and each of them once. A
	// host we have nothing on has nothing to tell us, and every one of these is an
	// SSH round trip.
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}

	sessions := map[string]map[string]string{} // host alias -> workspace -> session status
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue // config names a host it no longer has; treated as unreachable
		}
		var res agentproto.ListResult
		if err := callAgent(host, &res, "workspace-list"); err != nil {
			continue // unreachable; its workspaces are reported as such below
		}
		byName := map[string]string{}
		for _, ws := range res.Workspaces {
			byName[ws.Name] = ws.Status
		}
		sessions[alias] = byName
	}

	return mergeWorkspaceStatus(cfg.Workspaces, sessions), nil
}

// workspacesActivity asks each host once for the Claude attention state of the
// workspaces on it, and keeps only the ones this client owns (same rule as
// listWorkspaces — the host's directory may hold workspaces that aren't ours). A
// host we can't reach simply contributes nothing.
func workspacesActivity() (map[string]agentproto.Activity, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for _, alias := range cfg.Workspaces {
		needed[alias] = true
	}
	out := map[string]agentproto.Activity{}
	for alias := range needed {
		host := cfg.Hosts[alias]
		if host == nil {
			continue
		}
		var res agentproto.ActivityResult
		if err := callAgent(host, &res, "workspace-activity"); err != nil {
			continue // unreachable: its tabs just stay dim
		}
		for name, a := range res.Activity {
			if cfg.Workspaces[name] == alias { // ours, on this host
				out[name] = a
			}
		}
	}
	return out, nil
}

// mergeWorkspaceStatus is the decision, separated from the SSH so it can be tested:
// given the workspaces our config claims (name -> host alias) and what each host
// reported (alias -> name -> session status), what do we show?
//
// Only ours. A workspace the host has but our config doesn't is somebody else's, or
// was made by hand — and every command refuses to touch it anyway.
func mergeWorkspaceStatus(mine map[string]string, sessions map[string]map[string]string) []WorkspaceStatus {
	names := make([]string, 0, len(mine))
	for name := range mine {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]WorkspaceStatus, 0, len(names))
	for _, name := range names {
		alias := mine[name]
		status := agentproto.StatusUnreachable
		if byName, answered := sessions[alias]; answered {
			// The host answered and doesn't have it: it is gone — deleted from another
			// machine, most likely. Reporting "stopped" would be a lie you could act
			// on (there is nothing left to start).
			status = agentproto.StatusMissing
			if s, ok := byName[name]; ok {
				status = s
			}
		}
		out = append(out, WorkspaceStatus{Name: name, Host: alias, Status: status})
	}
	return out
}

func workspaceList() int {
	list, err := listWorkspaces()
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
		return workspaceClaude(target, rest)
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
func workspaceClaude(target sshx.Target, rest []string) int {
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
		return workspaceCheckpoint(target, session)
	default:
		return fail("usage: forge workspace <name> claude [renew|stop|checkpoint]")
	}
}

// checkpointMarker is the standalone line Claude is asked to print when the
// handoff is written. It is matched only as a whole trimmed line, so its mention
// inside the (echoed) prompt — mid-sentence — doesn't count.
const checkpointMarker = "FORGE_CHECKPOINT_SAVED"

// topicFile is where Claude is asked to leave a few words about what the session
// was about, for the resumed session to be named after.
//
// A file rather than a line on screen, deliberately. The pane also contains the
// echo of the prompt we typed — which necessarily mentions whatever token we
// would look for — so parsing the topic off the screen means telling Claude's
// answer apart from our own question, using text that wraps at whatever width the
// pane happens to be. A file has one writer and no echo.
const topicFile = "$HOME/.forge/checkpoint-topic"

// maxTopicLen bounds what ends up in a session name (and, before that, in a shell
// command). Claude is asked for a handful of words; this is the guard for when it
// answers with a paragraph.
const maxTopicLen = 60

// workspaceCheckpoint asks the running Claude session to write a handoff to its
// memory, waits for it to finish, then restarts the session so it continues from
// memory with a fresh context window. Run it from another terminal while the
// session is idle.
func workspaceCheckpoint(target sshx.Target, session string) int {
	if err := runCheckpoint(target, session, func(m string) { fmt.Println(m) }); err != nil {
		return fail("%v", err)
	}
	fmt.Println("done — fresh session running from memory. Reattach with: forge workspace <name> claude")
	return 0
}

// runCheckpoint is the transport-agnostic checkpoint: it asks the running Claude
// session to write a handoff to memory, waits for it to finish, then restarts the
// session so it continues from memory with a fresh context. Progress is reported
// through log so the CLI can print it and the browser UI can surface it. On any
// error the session is left running untouched (nothing killed) unless the error
// says otherwise.
func runCheckpoint(target sshx.Target, session string, log func(string)) error {
	if err := runCapture(target.Args("tmux", "has-session", "-t", session)); err != nil {
		return fmt.Errorf("no running claude session to checkpoint — start one first (forge workspace <name> claude, or the tab in forge ui)")
	}
	// Safe gate: only proceed when the pane is stable (no task streaming output).
	if !claudeIdle(target, session) {
		return fmt.Errorf("Claude looks busy — run checkpoint when it's idle (nothing running)")
	}
	// A marker already on screen is a leftover from an earlier checkpoint that
	// timed out (a successful one restarts the session, clearing it). Sending now
	// would match that stale line instantly and kill a session mid-work.
	if pane, ok := capturePane(target, session); ok && hasMarkerLine(pane, checkpointMarker) {
		return fmt.Errorf("a marker from an earlier checkpoint is still on screen — restart the session first " +
			"(forge workspace <name> claude stop && forge workspace <name> claude, or the restart button in forge ui)")
	}

	// Clear any topic left by an earlier checkpoint before asking for a new one, so
	// a Claude that ignores the request leaves us with nothing rather than with a
	// stale description of work that finished days ago.
	_ = runCapture(target.Args("mkdir -p \"$HOME/.forge\" && rm -f \"" + topicFile + "\""))

	// The marker is embedded mid-sentence (words before and after) so its echo in
	// the typed prompt can't wrap into a standalone marker line and false-positive;
	// Claude's own output prints it alone on a line, which is what we match.
	prompt := "Write a concise handoff to your memory right now — what we're working on, " +
		"the current state, and the exact next steps — so a brand-new session can continue " +
		"seamlessly. Do not ask me anything; just do it. Then write a single short line — " +
		"three to six words naming what this session is about, no punctuation at the end — " +
		"to the file " + topicFile + ", overwriting it. After the memory is fully written — " +
		"including any index or pointer file it needs — print the token " + checkpointMarker +
		" alone on its own line, as the very last thing you output, and then stop."
	log("→ asking Claude to write a handoff to memory…")
	if err := sendText(target, session, prompt); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}

	capture := func() (string, bool) { return capturePane(target, session) }
	if !waitForMarker(capture, checkpointMarker, panePoll, 3*time.Minute) {
		return fmt.Errorf("Claude didn't confirm the handoff in time — left the session running, nothing killed")
	}

	// The marker means "Claude believes it is done", not "Claude has stopped".
	// Asked to print it last, it may still print it mid-turn and go on writing —
	// the memory index, say. Killing on the marker alone truncates that write, and
	// the handoff we were preserving is the thing we corrupt. So wait for the pane
	// to actually fall quiet before killing anything.
	log("→ marker seen; waiting for Claude to go quiet…")
	if !waitQuiet(capture, panePoll, paneQuietFor, 2*time.Minute) {
		return fmt.Errorf("Claude kept working after the marker — left the session running, nothing killed")
	}

	// Read the topic before killing the session — after that the workspace is still
	// there, but there is no reason to leave it to chance.
	label := readTopic(target)
	if label == "" {
		// Claude didn't leave one (older session, or it just didn't). A timestamp
		// still distinguishes this checkpoint from the last one, which is the whole
		// point of naming them.
		label = time.Now().Format("2006-01-02 15:04")
	}
	log("→ handoff saved; restarting the session from memory…")
	_ = runCapture(target.Args("tmux", "kill-session", "-t", session))
	// target.User is the workspace name (WorkspaceTarget logs in as it).
	resume := agentproto.ResumeClaude(target.User, label)
	if err := runCapture(target.Args(resume)); err != nil {
		return fmt.Errorf("restart: %w (start it manually with: forge workspace <name> claude)", err)
	}
	return nil
}

// readTopic fetches the few words Claude left about what the session was about,
// and returns "" if there is nothing usable. Every failure here is soft: a
// checkpoint that worked must not be reported as failed because the session ended
// up with a duller name.
func readTopic(target sshx.Target) string {
	out, err := sshx.Capture(target.Args("cat \"" + topicFile + "\" 2>/dev/null || true")...)
	if err != nil {
		return ""
	}
	return sanitizeTopic(string(out))
}

// sanitizeTopic reduces whatever is in the file to one short, plain line. What
// Claude writes there is model output, and it is on its way into a shell command
// and a session name — so this keeps the first non-empty line, drops control
// characters, collapses runs of whitespace, and trims it to length.
func sanitizeTopic(s string) string {
	line := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	// Control characters (an ANSI escape, a stray CR) have no business in a name.
	line = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	line = strings.Join(strings.Fields(line), " ")
	// Models like to wrap a short answer in quotes or backticks; a name shouldn't
	// inherit them.
	line = strings.Trim(line, "\"'`*_#-— ")
	if len(line) > maxTopicLen {
		// Cut on a rune boundary, and prefer a word boundary when there is one near.
		line = strings.ToValidUTF8(line[:maxTopicLen], "")
		if i := strings.LastIndex(line, " "); i > maxTopicLen/2 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " ")
	}
	return line
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

// Pane polling: how often to sample, and how long the pane must hold still
// before we call Claude quiet. A streaming response redraws far faster than
// paneQuietFor, so a spinner or token stream keeps resetting the window.
const (
	panePoll     = 2 * time.Second
	paneQuietFor = 8 * time.Second
)

// claudeIdle reports whether the pane is stable across a short window — i.e. no
// task is streaming output. Version-independent (no reliance on TUI wording).
func claudeIdle(target sshx.Target, session string) bool {
	return waitQuiet(func() (string, bool) { return capturePane(target, session) },
		panePoll, panePoll, 3*panePoll)
}

// waitQuiet samples the pane until its contents stay unchanged for stableFor,
// which is what "Claude is not doing anything" actually looks like from outside:
// no wording to match, no version coupling. Returns false if it never settles
// within timeout, or if the pane can't be read.
//
// capture and poll are injected so the timing logic is testable without tmux or a
// server, and so the tests run in milliseconds rather than minutes.
func waitQuiet(capture func() (string, bool), poll, stableFor, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	last, ok := capture()
	if !ok {
		return false
	}
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		if time.Since(stableSince) >= stableFor {
			return true
		}
		time.Sleep(poll)
		cur, ok := capture()
		if !ok {
			return false
		}
		if cur != last {
			last, stableSince = cur, time.Now()
		}
	}
	return time.Since(stableSince) >= stableFor
}

// waitForMarker waits until the marker appears alone on a pane line.
func waitForMarker(capture func() (string, bool), marker string, poll, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pane, ok := capture(); ok && hasMarkerLine(pane, marker) {
			return true
		}
		time.Sleep(poll)
	}
	return false
}

// hasMarkerLine reports whether any line of s is the marker and nothing else.
//
// "Nothing else" is the whole trick. Claude Code decorates each of its output
// lines with a bullet — the marker arrives as "● FORGE_CHECKPOINT_SAVED", never
// bare — so an exact-equality check never matches, and every checkpoint runs to
// its timeout with the handoff written but the session never restarted. Leading
// decoration therefore has to be stripped.
//
// But only *decoration* may be stripped. The prompt we type mentions the token
// mid-sentence, and the pane echoes that prompt straight back; if a substring
// match were enough we would fire on our own prompt the instant we sent it and
// kill the session mid-work. So: strip the leading glyphs, then demand the rest
// of the line be exactly the marker — which the echoed sentence, with its words
// on either side, never is.
func hasMarkerLine(s, marker string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(stripDecoration(line)) == marker {
			return true
		}
	}
	return false
}

// stripDecoration drops leading whitespace and TUI glyphs (bullets, box-drawing,
// arrows) from a pane line, leaving it starting at its first real word.
func stripDecoration(line string) string {
	return strings.TrimLeftFunc(line, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
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
