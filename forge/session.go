package forge

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// The Claude session in a workspace, and the three things you can do to it from
// outside: stop it, restart it, or checkpoint it — save a handoff to memory and
// restart from that, which is the only one of the three that keeps the clocks
// running, because it is context compression rather than a new session.

// StopSession kills the workspace's Claude tmux session. An attached terminal
// sees its stream end; the session is gone from the server.
func StopSession(name string) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	// `|| true` inside KillClaude makes "no such session" a success (stopping an
	// already-stopped session is a no-op) while leaving a genuine failure — an
	// unreachable host — as an error. Swallowing every error would report "stopped"
	// for a server we never even reached.
	//
	// Clearing the tracking file in the same round trip ends the session's clocks: a
	// stop is the end of the session, so its start and time-present are gone (a fresh
	// session starts them over). A checkpoint, by contrast, keeps them.
	return runCapture(target, agentproto.KillClaude+"; "+agentproto.ClearSession)
}

// RestartSession hard-restarts the session: kill it, then start a fresh detached
// Claude. A terminal that reconnects attaches to the new session.
func RestartSession(name string) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	// Kill then relaunch in one round trip; the kill tolerates "no session", so a
	// restart also works as a start. Clear the tracking file too: a hard restart is a
	// new session with a new task, so its clocks start over (unlike a checkpoint).
	return runCapture(target, agentproto.KillClaude+"; "+agentproto.ClearSession+"; "+agentproto.StartClaude)
}

// Checkpoint asks the workspace's running Claude session to write a handoff to
// its memory, waits for it to finish, then restarts the session so it continues
// from memory with a fresh context window. Run it while the session is idle.
//
// It takes minutes, so the progress is the point: every step is written to out as
// it happens — the CLI prints those lines, the browser streams them.
func Checkpoint(name string, out io.Writer) error {
	target, err := workspaceTarget(name)
	if err != nil {
		return err
	}
	return checkpoint(target, agentproto.TmuxSession, out)
}

// workspaceTarget resolves a workspace name to the SSH target that logs into it.
//
// "Not created by this client" is the whole of why a name can be unknown while
// the workspace plainly exists on the server: Forge acts only on what its own
// config records. Same wording as DeleteWorkspace's, because it is the same
// refusal.
func workspaceTarget(name string) (sshx.Target, error) {
	cfg, err := loadConfig()
	if err != nil {
		return sshx.Target{}, err
	}
	host := cfg.HostFor(name)
	if host == nil {
		return sshx.Target{}, fmt.Errorf("unknown workspace %q — not created by this client", name)
	}
	return sshx.WorkspaceTarget(host, name), nil
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

// checkpoint is the body of Checkpoint, against a resolved session. Progress is
// written to out line by line, so a caller can show the same long run live rather
// than a spinner. On any error the session is left running untouched (nothing
// killed) unless the error says otherwise.
func checkpoint(target sshx.Target, session string, out io.Writer) error {
	log := func(m string) { fmt.Fprintln(out, m) }

	if err := runCapture(target, "tmux", "has-session", "-t", session); err != nil {
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
	_ = runCapture(target, "mkdir -p \"$HOME/.forge\" && rm -f \""+topicFile+"\"")

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
	// Pin the session's start into the tracking file before the kill: a checkpoint is
	// context compression, not a new session, so its clock must survive the restart
	// rather than adopting the fresh tmux session's creation time.
	_ = runCapture(target, agentproto.FreezeSession)
	_ = runCapture(target, "tmux", "kill-session", "-t", session)
	// target.User is the workspace name (WorkspaceTarget logs in as it).
	resume := agentproto.ResumeClaude(target.User, label)
	if err := runCapture(target, resume); err != nil {
		return fmt.Errorf("restart: %w (start it manually with: forge workspace <name> claude)", err)
	}
	return nil
}

// readTopic fetches the few words Claude left about what the session was about,
// and returns "" if there is nothing usable. Every failure here is soft: a
// checkpoint that worked must not be reported as failed because the session ended
// up with a duller name.
func readTopic(target sshx.Target) string {
	out, err := target.Output("cat \"" + topicFile + "\" 2>/dev/null || true")
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
	if err := target.Pipe(strings.NewReader(text), os.Stdout, os.Stderr,
		"tmux", "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if _, err := target.Output("tmux", "paste-buffer", "-d", "-b", buf, "-t", session); err != nil {
		return err
	}
	_, err := target.Output("tmux", "send-keys", "-t", session, "Enter")
	return err
}

// capturePane returns the visible pane text of the tmux session.
func capturePane(target sshx.Target, session string) (string, bool) {
	out, err := target.Output("tmux", "capture-pane", "-t", session, "-p")
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

// runCapture runs a remote command and keeps only whether it worked.
func runCapture(target sshx.Target, remote ...string) error {
	_, err := target.Output(remote...)
	return err
}
