package agent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeTurn puts a turn's files where the agent will look for them, with baseDir
// pointed at a directory of this test's own.
func writeTurn(t *testing.T, workspace, turn string) chatPaths {
	t.Helper()
	prev := baseDir
	baseDir = t.TempDir()
	t.Cleanup(func() { baseDir = prev })

	p := chatFiles(workspace, turn)
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

const aTurn = "20260805T142530.123456789"

// The last thing a turn writes is the thing a caller cannot do without.
//
// A follower has two questions — is there more, and is it over — and asking them
// in the wrong order loses the answer to the first. Check "is it over" after a
// read comes back empty, and a turn that finished in between is reported as
// finished having never had its final bytes read. Those bytes are the result
// message: the cost, the model, and the session id the next turn resumes from.
//
// The window is between the read and the decision, so the test stands in it. A
// test that wrote the last line and the marker while the follower slept would
// pass against either order and prove nothing — which is what the first version
// of this test did.
func TestATurnThatEndsMidReadStillDeliversItsLastLine(t *testing.T) {
	p := writeTurn(t, "ws", aTurn)

	first := `{"type":"assistant","text":"thinking"}` + "\n"
	last := `{"type":"result","total_cost_usd":0.01,"session_id":"s-1"}` + "\n"
	if err := os.WriteFile(p.stream, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}

	// The turn ends exactly once, the first time the follower comes up empty:
	// its final write, then its exit status, in the order a shell does them.
	var once sync.Once
	reads := 0
	defer stubAfterRead(t, func() {
		reads++
		if reads < 2 {
			return // the first read is the one that catches up on `first`
		}
		once.Do(func() {
			if err := os.WriteFile(p.stream, []byte(first+last), 0o600); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(p.done, []byte("0"), 0o600); err != nil {
				t.Error(err)
			}
		})
	})()

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followTurn(p, 0, &out) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("following the turn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never returned, though the turn is marked done")
	}

	if got := out.String(); got != first+last {
		t.Errorf("the reader got:\n%q\nwant:\n%q\n— the result line is what carries "+
			"the cost, the model and the session the next turn resumes from", got, first+last)
	}
}

// stubAfterRead puts a hook in the follower's window and hands back the undo.
func stubAfterRead(t *testing.T, f func()) func() {
	t.Helper()
	prev := afterRead
	afterRead = f
	return func() { afterRead = prev }
}

// Reconnecting is exact, not approximate: a reader that got 40 bytes in asks for
// the rest and gets the rest, with nothing repeated and nothing skipped. That is
// the whole reason the stream is a file — the reader's entire state is one
// integer, which is what survives a phone going into a tunnel.
func TestAReaderResumesExactlyWhereItStopped(t *testing.T) {
	p := writeTurn(t, "ws", aTurn)

	stream := strings.Repeat(`{"type":"assistant","text":"x"}`+"\n", 20)
	if err := os.WriteFile(p.stream, []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.done, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}

	var whole bytes.Buffer
	if err := followTurn(p, 0, &whole); err != nil {
		t.Fatal(err)
	}
	if whole.String() != stream {
		t.Fatalf("reading a finished turn from the start gave %d bytes, want %d",
			whole.Len(), len(stream))
	}

	const stopped = 40
	var rest bytes.Buffer
	if err := followTurn(p, stopped, &rest); err != nil {
		t.Fatal(err)
	}
	if got, want := rest.String(), stream[stopped:]; got != want {
		t.Errorf("resuming at %d gave %q, want %q", stopped, got, want)
	}
}

// A turn that has not written anything yet is not a turn that has failed. The
// gap between starting one and Claude Code's first line is the ordinary case,
// and a follower has to wait through it rather than report a missing file.
func TestFollowingATurnThatHasNotWrittenYet(t *testing.T) {
	p := writeTurn(t, "ws", aTurn)

	var out safeBuffer
	done := make(chan error, 1)
	go func() { done <- followTurn(p, 0, &out) }()

	select {
	case err := <-done:
		t.Fatalf("the follower gave up before the turn said anything (%v)", err)
	case <-time.After(3 * chatPoll):
	}

	line := `{"type":"system","subtype":"init"}` + "\n"
	if err := os.WriteFile(p.stream, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.done, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never returned")
	}
	if out.String() != line {
		t.Errorf("got %q, want %q", out.String(), line)
	}
}

// Turn ids arrive from off this machine and become path elements. The check is a
// whitelist of what this package produces, so the question is not "does it
// contain .." but "is it one of ours".
func TestATurnIdIsOnlyEverOneOfOurs(t *testing.T) {
	if !turnRe.MatchString(turnID(time.Now())) {
		t.Errorf("this package makes turn ids its own check rejects: %q", turnID(time.Now()))
	}
	for _, bad := range []string{
		"", "..", "../../etc/passwd",
		"20260805T142530.123456789/../..",
		"20260805T142530.123456789 ; rm -rf /",
		"20260805T142530.12345678",   // a digit short
		"20260805T142530.1234567890", // and one too many
		"20260805t142530.123456789",
		"/absolute/20260805T142530.123456789",
	} {
		if turnRe.MatchString(bad) {
			t.Errorf("accepted %q as a turn id", bad)
		}
	}
}

// Turn ids sort in the order the turns happened, because a directory listing is
// how anything will ever ask "what came before this".
func TestTurnIdsSortByWhenTheyRan(t *testing.T) {
	base := time.Date(2026, 8, 5, 14, 25, 30, 0, time.UTC)
	var prev string
	for _, step := range []time.Duration{0, time.Nanosecond, time.Second, 90 * time.Minute, 24 * time.Hour} {
		id := turnID(base.Add(step))
		if prev != "" && !(prev < id) {
			t.Errorf("%q does not sort before %q", prev, id)
		}
		prev = id
	}
}

// The command that runs inside tmux, held to what each flag is there for. None
// of this can be checked by running it — there is no Claude Code in CI — so it
// is checked as the string it is.
func TestTheTurnCommandIsHeadlessStreamingClaude(t *testing.T) {
	p := chatFiles("ws", aTurn)

	fresh := turnCommand(p, "")
	for _, want := range []string{
		"claude -p ",
		"--output-format stream-json",
		"--verbose",                  // without it stream-json says only the result
		"--include-partial-messages", // without it a chat is a wait
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("the turn command is missing %q:\n%s", want, fresh)
		}
	}
	// --bare skips OAuth and the workspace's own settings: a different login, and
	// no bypassPermissions. It must never appear here.
	if strings.Contains(fresh, "--bare") {
		t.Error("the turn command passes --bare, which skips OAuth and settings")
	}
	// A first turn has nothing to resume, and --resume with an empty id is not the
	// same request.
	if strings.Contains(fresh, "--resume") {
		t.Errorf("a first turn asked to resume something:\n%s", fresh)
	}

	// The exit status is the marker a reader waits on, so it has to be written
	// after the output and whatever the exit code was.
	if i, j := strings.Index(fresh, p.stream), strings.LastIndex(fresh, p.done); i < 0 || j < i {
		t.Errorf("the done marker is not written after the stream:\n%s", fresh)
	}

	resumed := turnCommand(p, "s-1")
	if !strings.Contains(resumed, "--resume 's-1'") {
		t.Errorf("a resumed turn does not continue the conversation:\n%s", resumed)
	}
	// The session id comes out of Claude Code's own output. It has always been a
	// uuid, which is not a reason to hand it to a shell unquoted.
	if got := turnCommand(p, "a'b"); !strings.Contains(got, `'a'\''b'`) {
		t.Errorf("a session id with a quote in it escapes the command:\n%s", got)
	}
}

// The three files of a turn share a stem, and all of them live under the
// workspace's own hidden directory rather than anywhere a file tree would show.
func TestATurnsFilesAreOneStemInTheWorkspace(t *testing.T) {
	prev := baseDir
	baseDir = "/home/workspaces"
	t.Cleanup(func() { baseDir = prev })

	p := chatFiles("ws", aTurn)
	stem := filepath.Join("/home/workspaces/ws/.claude/forge-chat", aTurn)
	for name, got := range map[string]string{
		"prompt": p.prompt, "stream": p.stream, "done": p.done, "stderr": p.errFile,
	} {
		if !strings.HasPrefix(got, stem+".") {
			t.Errorf("the %s file is %q, which does not share the turn's stem %q", name, got, stem)
		}
	}
	// stderr apart from the stream, because one line of it in the middle of the
	// NDJSON is a transcript nothing can parse.
	if p.errFile == p.stream {
		t.Error("stderr goes to the stream, where it would corrupt the transcript")
	}
}

// safeBuffer is a bytes.Buffer the test can read while the follower writes.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the follower to catch up")
		}
		time.Sleep(chatPoll / 3)
	}
}

// A turn that was never started here is said so, not waited for.
//
// A stream that does not exist is the ordinary state of a turn a moment old, so
// the follower is right to wait through it — which means a wrong name or a wrong
// id would be waited through too, for as long as anyone let it. The prompt is
// what tells the two apart: it is written before the id is handed out, so a turn
// with no prompt is a turn this host never ran.
func TestTailingATurnThatWasNeverStarted(t *testing.T) {
	writeTurn(t, "ws", aTurn)

	done := make(chan int, 1)
	go func() { done <- opChatTail([]string{"-name", "ws", "-turn", aTurn}) }()

	select {
	case code := <-done:
		if code == 0 {
			t.Error("tailing a turn that does not exist succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tailing a turn that was never started waited for it instead of " +
			"saying so — and would have waited as long as anyone let it")
	}
}

// This command's stdout carries Claude Code's stream verbatim, so nothing else
// may be written there. A JSON object about a failure would arrive as one more
// line of the conversation, and whatever is reading it has every reason to
// believe it.
func TestTheTailCommandKeepsItsComplaintsOffTheStream(t *testing.T) {
	writeTurn(t, "ws", aTurn)

	out := captureStdout(t)
	code := opChatTail([]string{"-name", "ws", "-turn", "not-a-turn-id"})
	if code == 0 {
		t.Error("an invalid turn id was accepted")
	}
	if got := out(); got != "" {
		t.Errorf("the tail command wrote %q to the stream; failures belong on stderr", got)
	}
}

// captureStdout swaps os.Stdout for a pipe and hands back a function that
// restores it and reports what was written.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w

	read := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		read <- b.String()
	}()

	return func() string {
		os.Stdout = prev
		_ = w.Close()
		s := <-read
		_ = r.Close()
		return s
	}
}
