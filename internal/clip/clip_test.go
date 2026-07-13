package clip

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// newTestFilter wraps a buffer and records what would have gone to the clipboard,
// so the parser can be tested without touching the machine's real one.
func newTestFilter(out *bytes.Buffer, fail bool) (*Filter, *[]string) {
	var copied []string
	f := NewFilter(out)
	f.copy = func(s string) error {
		if fail {
			return errNoClipboard
		}
		copied = append(copied, s)
		return nil
	}
	return f, &copied
}

func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

func TestFilterCopiesAndRemovesTheSequence(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)

	in := "before " + osc52("https://claude.com/oauth?code=1") + "after"
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}

	if got := out.String(); got != "before after" {
		t.Errorf("stream = %q, want the escape removed", got)
	}
	if len(*copied) != 1 || (*copied)[0] != "https://claude.com/oauth?code=1" {
		t.Errorf("copied = %v", *copied)
	}
}

// The escape does not arrive in one read: ssh hands us whatever the network gave
// it. Split at every byte and the result must not change.
func TestFilterHandlesEverySplit(t *testing.T) {
	in := "x" + osc52("hello") + "y"
	for cut := 0; cut <= len(in); cut++ {
		var out bytes.Buffer
		f, copied := newTestFilter(&out, false)
		if _, err := f.Write([]byte(in[:cut])); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(in[cut:])); err != nil {
			t.Fatal(err)
		}
		if err := f.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "xy" {
			t.Errorf("split at %d: stream = %q, want %q", cut, got, "xy")
		}
		if len(*copied) != 1 || (*copied)[0] != "hello" {
			t.Errorf("split at %d: copied = %v", cut, *copied)
		}
	}
}

// ST (ESC \) terminates an OSC just as BEL does; tmux uses it in some paths.
func TestFilterAcceptsStringTerminator(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)
	in := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("st-form")) + "\x1b\\!"
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "!" {
		t.Errorf("stream = %q", got)
	}
	if len(*copied) != 1 || (*copied)[0] != "st-form" {
		t.Errorf("copied = %v", *copied)
	}
}

// Everything that is not an OSC 52 must come out byte-for-byte: a terminal
// session is nothing but escape sequences, and eating one would corrupt the
// display of the very session we are trying to make usable.
func TestFilterPassesOtherEscapesThrough(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)
	in := "\x1b[1;32mgreen\x1b[0m\x1b]0;a title\x07\x1b]12;red\x07"
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != in {
		t.Errorf("stream = %q, want it unchanged", got)
	}
	if len(*copied) != 0 {
		t.Errorf("nothing should have been copied, got %v", *copied)
	}
}

// A clipboard we cannot reach must not swallow the escape: a terminal that does
// support OSC 52 still deserves its chance. Strictly better, never worse.
func TestFilterForwardsWhenTheClipboardIsUnreachable(t *testing.T) {
	var out bytes.Buffer
	f, _ := newTestFilter(&out, true)
	in := osc52("fallback")
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != in {
		t.Errorf("stream = %q, want the escape forwarded untouched", got)
	}
}

// "?" asks to READ the clipboard. Answering it would hand a session running
// Claude with permission prompts off whatever you last copied.
func TestFilterIgnoresClipboardReads(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)
	if _, err := f.Write([]byte("\x1b]52;c;?\x07")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Errorf("stream = %q, want the read request dropped", got)
	}
	if len(*copied) != 0 {
		t.Errorf("a read request must not copy anything, got %v", *copied)
	}
}

func TestFilterRefusesAnOversizedPayload(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)
	huge := "\x1b]52;c;" + strings.Repeat("A", maxPayload+10) + "\x07"
	if _, err := f.Write([]byte(huge)); err != nil {
		t.Fatal(err)
	}
	if len(*copied) != 0 {
		t.Errorf("an oversized payload must not be copied")
	}
	if f.pending != nil {
		t.Errorf("an oversized payload must not be buffered, still holding %d bytes", len(f.pending))
	}
}

// The only shape an over-the-cap payload can actually have: too big to arrive in
// one read. The cap must not lose track of the sequence between reads — decide
// once, then keep forwarding until it ends. Get this wrong and the tail of the
// base64 (and its BEL) is printed onto the user's screen as if it were output.
func TestFilterDoesNotLeakAnOversizedPayloadSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	f, copied := newTestFilter(&out, false)

	head := "\x1b]52;c;" + strings.Repeat("A", maxPayload+10)
	tail := strings.Repeat("B", 64) + "\x07"
	if _, err := f.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(tail + "prompt$ ")); err != nil {
		t.Fatal(err)
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}

	if len(*copied) != 0 {
		t.Errorf("an oversized payload must not be copied")
	}
	// Everything is forwarded as the escape sequence it is — a terminal that
	// handles OSC 52 sees a well-formed sequence, and one that does not discards
	// it. What must never happen is the payload arriving as ordinary output.
	want := head + tail + "prompt$ "
	if got := out.String(); got != want {
		t.Errorf("stream mangled: got %d bytes, want %d (the sequence forwarded whole, then the prompt)",
			len(got), len(want))
	}
	if strings.Contains(out.String()[len(head):], "\x07prompt$ ") != true {
		t.Errorf("the sequence must end at its terminator, with the prompt after it")
	}
}

// A dead terminal must be reported, not papered over. Swallowing the error and
// buffering for a writer that will never take another byte grows `pending`
// forever and tells the copier everything is fine.
func TestFilterReportsWriteErrors(t *testing.T) {
	boom := errors.New("terminal is gone")
	f := NewFilter(errWriter{boom})
	f.copy = func(string) error { return errNoClipboard } // force the forward path

	if _, err := f.Write([]byte(osc52("hi"))); !errors.Is(err, boom) {
		t.Errorf("Write error = %v, want %v", err, boom)
	}
	if _, err := f.Write([]byte("plain output")); !errors.Is(err, boom) {
		t.Errorf("Write error on ordinary output = %v, want %v", err, boom)
	}
	if len(f.pending) != 0 {
		t.Errorf("a failed write must not stash %d bytes for a writer that is gone", len(f.pending))
	}
}

type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }
