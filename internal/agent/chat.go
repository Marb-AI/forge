package agent

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// A chat turn, on the host.
//
// The other way to work with Claude here is a terminal: a tmux session running
// the TUI, which the browser attaches to and draws. It is the right answer on a
// laptop and no answer at all on a phone, where the software keyboard eats the
// screen and a redraw over a mobile link is a redraw you watch happen.
//
// So there is a second shape, and the whole of it is here: a prompt goes in, and
// what Claude Code says comes out as stream-json — the same binary, the same
// login, no API key and no second agent framework. What this file adds is where
// that stream goes.
//
// It goes to a file. Not to whoever asked, because on a phone that reader is
// gone by the second paragraph, and not through tmux's screen, because a TUI is
// a rendering and reading it back means parsing ANSI, redraws and spinners that
// change every release. A file is written once and read as often as anyone
// likes, from wherever they got to; the reader's entire state is an integer.
// That is what makes a turn survive a tunnel, and what makes reconnecting exact
// rather than approximate.
//
// tmux stays, with one job: keeping the process alive when the connection that
// started it ends. Nothing here reads its screen.

// turnRe is what a turn id may look like — timestamps this package produced, and
// nothing else. Turn ids arrive from off this machine and become path elements,
// so the check is a whitelist rather than a search for the ways out of a
// directory.
var turnRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}$`)

// turnID names a turn by when it started: sortable as a string, unique to the
// nanosecond, and readable by whoever is looking at the directory. UTC, because
// the alternative is a directory that reorders itself twice a year.
func turnID(t time.Time) string {
	return t.UTC().Format("20060102T150405.000000000")
}

// chatPaths are one turn's files. Held together because everything here needs
// two or three of them at once and computing them apart is how a stem drifts.
type chatPaths struct {
	dir     string // the workspace's chat directory
	prompt  string
	stream  string
	done    string
	errFile string
	session string
}

func chatFiles(workspace, turn string) chatPaths {
	home := filepath.Join(baseDir, workspace)
	dir := filepath.Join(home, agentproto.ChatDir)
	stem := filepath.Join(dir, turn)
	return chatPaths{
		dir:     dir,
		prompt:  stem + agentproto.ChatPromptSuffix,
		stream:  stem + agentproto.ChatStreamSuffix,
		done:    stem + agentproto.ChatDoneSuffix,
		errFile: stem + agentproto.ChatErrSuffix,
		session: filepath.Join(home, agentproto.ChatSessionFile),
	}
}

// turnCommand is what runs inside tmux: Claude Code in headless mode, its stream
// to one file and its complaints to another, and the exit status recorded when
// it is over.
//
// Every flag is load-bearing.
//
//   - -p is headless: one prompt, one answer, no TUI to render.
//   - --output-format stream-json is the point of the exercise: tool calls, cost
//     and per-model attribution arrive as data rather than as something to read
//     back off a screen.
//   - --verbose is required for stream-json to say anything but the result.
//   - --include-partial-messages is what makes it a chat rather than a wait: the
//     text arrives as it is written.
//   - --resume continues the conversation. Claude Code keeps the history on this
//     host; the transcripts here are for the reader, not for it.
//
// And one flag that must never appear: --bare skips OAuth and the workspace's
// own settings, which means a different login and no bypassPermissions.
//
// The prompt comes in on stdin rather than in argv: it is user text of any
// length, and argv is both bounded and visible in ps to every account on the
// host.
func turnCommand(p chatPaths, resume string) string {
	claude := "claude -p --output-format stream-json --verbose --include-partial-messages"
	if resume != "" {
		claude += " --resume " + shellQuote(resume)
	}
	// The exit status is written whatever happens, and last: it is the marker a
	// reader waits on, so it must not exist while there is still output coming.
	return agentproto.SourceEnv +
		claude +
		" <" + shellQuote(p.prompt) +
		" >" + shellQuote(p.stream) +
		" 2>" + shellQuote(p.errFile) +
		"; printf '%s' \"$?\" >" + shellQuote(p.done)
}

// shellQuote wraps a string for /bin/sh single quotes. Paths here are built from
// a validated workspace name and a validated turn id, but the session id comes
// out of Claude Code's own output, and "it has always been a uuid" is not a
// reason to hand it to a shell unquoted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// opChatSend starts a turn and prints its id. The prompt is this process's stdin.
func opChatSend(args []string) int {
	fs := flag.NewFlagSet("claude-chat-send", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	if _, err := os.Stat(filepath.Join(baseDir, *name)); err != nil {
		return emitError("no workspace %q on this host", *name)
	}

	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return emitError("read prompt: %v", err)
	}
	if len(strings.TrimSpace(string(prompt))) == 0 {
		return emitError("the prompt is empty")
	}

	// One turn at a time, and tmux is what knows: a second prompt while Claude is
	// still answering the first would be two processes resuming one conversation,
	// and whichever finished last would decide what the history says.
	if chatTurnRunning(*name) {
		return emitError("a turn is already running in %q", *name)
	}

	turn := turnID(time.Now())
	p := chatFiles(*name, turn)
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return emitError("make %s: %v", p.dir, err)
	}
	if err := os.WriteFile(p.prompt, prompt, 0o600); err != nil {
		return emitError("write prompt: %v", err)
	}
	// The agent is root and Claude is not, so everything just created has to be
	// handed over — including ~/.claude itself, which MkdirAll will have made on a
	// workspace that has never run Claude, and which the chat directory alone does
	// not cover.
	//
	// Checked rather than attempted: a turn whose prompt Claude cannot read starts,
	// writes nothing, and ends. The caller would have a turn id for it and would
	// wait out a conversation that never happened, which is a worse answer than
	// the failure.
	owner := *name + ":" + *name
	if out, err := run("chown", owner,
		filepath.Dir(p.dir), p.dir, p.prompt); err != nil {
		return emitError("hand the turn to %s: %v: %s", *name, err, out)
	}

	resume, _ := os.ReadFile(p.session)
	cmd := turnCommand(p, strings.TrimSpace(string(resume)))
	if out, err := run("runuser", "-l", *name, "-c",
		"tmux new -d -s "+agentproto.ChatTmuxSession+" "+shellQuote(cmd)); err != nil {
		return emitError("start turn: %v: %s", err, out)
	}
	return emit(agentproto.ChatTurn{Turn: turn})
}

// chatTurnRunning reports whether this workspace has a turn in flight.
func chatTurnRunning(name string) bool {
	_, err := run("runuser", "-l", name, "-c",
		"tmux has-session -t "+agentproto.ChatTmuxSession+" 2>/dev/null")
	return err == nil
}

// opChatTail writes a turn's stream to stdout from an offset and follows it
// until the turn is over.
//
// Raw, not wrapped: what it prints is what Claude Code printed, so a caller can
// hand the bytes to a browser without unpacking them from something first. That
// makes stdout unavailable for saying anything went wrong, which is why failures
// here go to stderr and are reported by the exit status — the same arrangement
// the file browser's remote snippets already use.
func opChatTail(args []string) int {
	fs := flag.NewFlagSet("claude-chat-tail", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	turn := fs.String("turn", "", "turn id")
	offset := fs.Int64("offset", 0, "resume from this many bytes in")
	if err := fs.Parse(args); err != nil {
		return tailError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return tailError("invalid workspace name %q", *name)
	}
	if !turnRe.MatchString(*turn) {
		return tailError("invalid turn id %q", *turn)
	}
	if *offset < 0 {
		return tailError("offset %d is before the start", *offset)
	}

	p := chatFiles(*name, *turn)
	// The prompt is written before the turn id is ever handed out, so its absence
	// means this turn was never started here — a name or an id that does not
	// belong to this host. Worth one stat: without it the follower would wait for
	// a stream that is not late but imaginary, and wait for as long as anyone let
	// it, because a turn yet to write its first line looks exactly the same.
	if !exists(p.prompt) {
		return tailError("no turn %s in %q on this host", *turn, *name)
	}
	if err := followTurn(p, *offset, os.Stdout); err != nil {
		return tailError("%v", err)
	}
	return 0
}

// tailError is emitError for the one command whose stdout is not ours: it is
// carrying Claude Code's stream verbatim, so a JSON object about a failure
// printed there would arrive as one more line of the conversation. Stderr and
// the exit status say it instead — the same arrangement the file browser's
// remote snippets use.
func tailError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "forge-agent: "+format+"\n", args...)
	return 1
}

// chatPoll is how often a follower looks for more. Short enough that a sentence
// arrives as one, long enough that a turn spent thinking costs nothing: the file
// is local and a stat that finds nothing is the cheapest syscall here.
const chatPoll = 150 * time.Millisecond

// followTurn copies the stream from offset to w, and keeps copying until the
// turn is finished and there is nothing left.
//
// The order of the two checks is the whole correctness of this: the done marker
// is read FIRST and acted on after a final read. A turn that finishes between a
// read returning nothing and the check for the marker would otherwise lose
// whatever it wrote in between — which is the last thing it wrote, the result,
// the one line a caller cannot do without.
func followTurn(p chatPaths, offset int64, w io.Writer) error {
	for {
		finished := exists(p.done)

		n, err := copyFrom(p.stream, offset, w)
		if err != nil {
			return err
		}
		offset += n

		afterRead()

		if finished {
			return nil
		}
		if n == 0 {
			time.Sleep(chatPoll)
		}
	}
}

// afterRead is where a turn is allowed to finish underneath the follower.
//
// It exists because the bug this guards against is an interleaving, and an
// interleaving cannot be provoked from outside: the window is between the read
// above and the decision below, which is microseconds wide and hit by a real
// turn perhaps once in a great many. A test that wrote the last line and the
// marker while the follower slept would pass against both orders and prove
// nothing. So the test gets to stand in that window.
var afterRead = func() {}

// copyFrom writes the file's contents from offset onwards and reports how many
// bytes that was. A stream that does not exist yet is not an error: the turn was
// started a moment ago and Claude Code has not written its first line.
func copyFrom(path string, offset int64, w io.Writer) (int64, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.Copy(w, f)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
