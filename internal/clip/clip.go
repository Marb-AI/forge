// Package clip carries text out of a remote session and into the local
// clipboard, because nothing else in the chain can be relied on to do it.
//
// A workspace is a headless Linux box: no X, no Wayland, no xclip, DISPLAY
// unset. Nothing there has a clipboard. So everything that copies — Claude's
// "press c" on a login URL, a tmux copy-mode yank — hands the text to the
// *terminal* as an OSC 52 escape and trusts the terminal to finish the job.
//
// Terminals do not. macOS Terminal.app has never implemented OSC 52 and offers
// no setting to change that. Warp implemented it for years, then classified it
// as a vulnerability (CVE-2026-48725: a remote host could silently read or
// overwrite your clipboard) and now defaults it to deny. iTerm2 ships it off.
// Ghostty, WezTerm and kitty allow it. So "copy the URL out of your session" is
// a coin flip on which terminal the user happens to run — and Forge's own
// tmux.conf, which carefully forwards the escape, was betting on that flip.
//
// So Forge stops betting: it reads its own SSH output, sees the escape itself,
// and puts the text on the clipboard with the local OS tool. Any terminal, same
// behaviour. This is the same job the browser UI does with navigator.clipboard.
package clip

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os/exec"
	"runtime"
	"time"
)

// maxPayload caps an OSC 52 payload before we decode it. A session's output is
// untrusted — Claude runs there unattended, and a runaway command's bytes are
// terminal output like any other. A copy is a URL, a snippet, a stack trace; a
// megabyte of base64 is not, and decoding it to find that out is exactly the
// work we should not be tricked into.
const maxPayload = 1 << 20

// Filter wraps a terminal's output stream: everything is forwarded untouched
// except a complete OSC 52 clipboard-write, which is handled locally and removed
// from the stream — so a terminal that would refuse it (Warp, which now shows a
// warning banner) or ignore it (Terminal.app) never sees it at all.
//
// If the local clipboard cannot be reached, the escape is forwarded instead of
// swallowed: a terminal that *does* support OSC 52 still gets its chance, and we
// are strictly better than we were, never worse.
type Filter struct {
	out  io.Writer
	copy func(string) error

	// pending holds a partial escape sequence: OSC 52 can be split across reads,
	// and a sequence we have not finished reading is one we cannot yet judge.
	pending []byte

	// A payload past maxPayload is not buffered to judge later — it is forwarded
	// as it arrives and forgotten. Deciding that mid-sequence means the decision
	// has to survive until the sequence ends, which is a later read: draining says
	// we are inside such a sequence, lastEsc that its final byte might be an ST.
	draining bool
	lastEsc  bool
}

// NewFilter wraps out. Writes to the returned Filter reach out unchanged, minus
// any OSC 52 clipboard-write it handles.
func NewFilter(out io.Writer) *Filter {
	return &Filter{out: out, copy: Copy}
}

const (
	esc = 0x1b
	bel = 0x07
)

var oscPrefix = []byte{esc, ']', '5', '2', ';'}

func (f *Filter) Write(p []byte) (int, error) {
	// Report the caller's whole write as consumed on success: what we hold back is
	// buffered, not lost, and a short count would read as a write error to the
	// copier. A real write error, though, is returned — the terminal being gone is
	// not something to keep quiet about while buffering data for it forever.
	buf := p
	if len(f.pending) > 0 {
		buf = append(f.pending, p...)
		f.pending = nil
	}

	for len(buf) > 0 {
		if f.draining {
			rest, err := f.drain(buf)
			if err != nil {
				return len(p), err
			}
			buf = rest
			continue
		}

		i := bytes.IndexByte(buf, esc)
		if i < 0 {
			_, err := f.out.Write(buf)
			return len(p), err
		}
		// Everything before the escape is ordinary output.
		if i > 0 {
			if _, err := f.out.Write(buf[:i]); err != nil {
				return len(p), err
			}
			buf = buf[i:]
		}

		rest, done, err := f.escape(buf)
		if err != nil {
			return len(p), err
		}
		if !done {
			// An incomplete sequence: hold it and wait for the next read rather
			// than guess. A terminal would do the same.
			f.pending = append(f.pending[:0], buf...)
			return len(p), nil
		}
		buf = rest
	}
	return len(p), nil
}

// escape handles the escape sequence at the head of buf. It returns the rest of
// buf and whether the sequence was complete; an incomplete one is left for the
// next read.
func (f *Filter) escape(buf []byte) (rest []byte, done bool, err error) {
	// Not enough bytes yet to know whether this is an OSC 52 at all.
	n := min(len(buf), len(oscPrefix))
	if !bytes.Equal(buf[:n], oscPrefix[:n]) {
		// Some other escape sequence — none of our business. Emit the ESC and let
		// the scan continue after it, so a later OSC 52 is still found.
		_, err := f.out.Write(buf[:1])
		return buf[1:], true, err
	}
	if len(buf) < len(oscPrefix) {
		return buf, false, nil // still could become an OSC 52
	}

	payload, rest, term := splitTerminator(buf[len(oscPrefix):])
	if !term {
		if len(payload) > maxPayload {
			// Past the cap with no end in sight. We will not buffer a sequence this
			// big to judge it later — but we will not eat it either: forward what we
			// have and keep forwarding until it ends, so a terminal that handles OSC
			// 52 itself (Ghostty, kitty) still sees a well-formed sequence rather than
			// a screenful of base64. Copying it is off the table; passing it on is not.
			f.draining, f.lastEsc = true, false
			_, err := f.out.Write(buf)
			return nil, true, err
		}
		return buf, false, nil
	}
	copied, err := f.handle(payload)
	if err != nil {
		return nil, true, err
	}
	if !copied {
		// We could not copy it — forward the sequence untouched so a terminal that
		// can handle OSC 52 still gets the chance.
		seq := buf[:len(buf)-len(rest)]
		_, err := f.out.Write(seq)
		return rest, true, err
	}
	return rest, true, nil
}

// drain forwards the tail of an over-sized sequence, up to and including its
// terminator, and then returns to ordinary scanning.
func (f *Filter) drain(buf []byte) (rest []byte, err error) {
	for i, b := range buf {
		if b == bel || (f.lastEsc && b == '\\') {
			f.draining, f.lastEsc = false, false
			_, err := f.out.Write(buf[:i+1])
			return buf[i+1:], err
		}
		f.lastEsc = b == esc
	}
	_, err = f.out.Write(buf)
	return nil, err
}

// handle copies an OSC 52 payload ("<selection>;<base64>") to the clipboard. It
// reports whether the text is now on the clipboard — and so whether the sequence
// can be dropped from the stream. Anything we could not copy is left for the
// terminal to try; the one exception is a clipboard *read*, which we refuse on
// the terminal's behalf too.
func (f *Filter) handle(payload []byte) (copied bool, err error) {
	i := bytes.IndexByte(payload, ';')
	if i < 0 {
		return false, nil // malformed: let the terminal make of it what it will
	}
	data := payload[i+1:]
	// "?" is the session asking to *read* the clipboard. We do not answer it, and
	// we do not pass it on to a terminal that might: Claude runs in these sessions
	// with permission prompts off, and a session that can read your clipboard can
	// read whatever you last copied — a password, a token. This is the half of
	// OSC 52 that got Warp a CVE.
	if len(data) == 0 || bytes.Equal(data, []byte("?")) {
		return true, nil
	}
	if len(data) > maxPayload {
		return false, nil
	}
	text, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return false, nil // not base64: there is nothing we can put anywhere
	}
	return f.copy(string(text)) == nil, nil
}

// splitTerminator finds the end of an OSC payload: BEL, or ST (ESC \).
func splitTerminator(b []byte) (payload, rest []byte, found bool) {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case bel:
			return b[:i], b[i+1:], true
		case esc:
			if i+1 >= len(b) {
				return b[:i], nil, false // ST may still be coming
			}
			if b[i+1] == '\\' {
				return b[:i], b[i+2:], true
			}
		}
	}
	return b, nil, false
}

// Flush emits any partial escape sequence still held back, so output that merely
// looks like the start of one is not lost when the session ends.
func (f *Filter) Flush() error {
	if len(f.pending) == 0 {
		return nil
	}
	_, err := f.out.Write(f.pending)
	f.pending = nil
	return err
}

// copyTimeout bounds the clipboard tool. The copy runs on the goroutine draining
// the session's output, so a tool that blocks blocks the screen: Claude would
// look frozen because pbcopy was. Bounded, the worst case is a copy that does not
// happen — and the escape is then forwarded, so the terminal still gets its try.
const copyTimeout = 3 * time.Second

// Copy puts text on the local clipboard using the OS tool. A machine with no
// such tool is not an error worth shouting about — the caller forwards the
// escape instead, and the terminal can try.
func Copy(text string) error {
	name, args := clipboardCmd()
	if name == "" {
		return errNoClipboard
	}
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader([]byte(text))
	return cmd.Run()
}

type clipError string

func (e clipError) Error() string { return string(e) }

const errNoClipboard = clipError("no clipboard tool on this machine")

// clipboardCmd picks the local clipboard writer. Wayland before X11 (a Wayland
// session often still has xclip, pointing at an X server nobody is looking at).
func clipboardCmd() (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "pbcopy", nil
	case "windows":
		return "clip", nil
	default:
		for _, c := range [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		} {
			if p, err := exec.LookPath(c[0]); err == nil {
				return p, c[1:]
			}
		}
		return "", nil
	}
}
