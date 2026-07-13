package clip

import (
	"bytes"
	"encoding/base64"
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
