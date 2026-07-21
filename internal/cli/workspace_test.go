package cli

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHasMarkerLine(t *testing.T) {
	const m = "FORGE_CHECKPOINT_SAVED"
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"alone on a line", "working…\n" + m + "\n", true},
		{"indented by the TUI", "  " + m + "  \n", true},
		{"mid-sentence in the echoed prompt", "print the token " + m + " alone on its own line\n", false},
		{"as a prefix", m + "_LATER\n", false},
		{"absent", "still thinking\n", false},

		// How the marker ACTUALLY arrives. Claude Code bullets every line it
		// prints, so the marker is never bare — and matching on bare equality
		// meant no checkpoint ever saw it: each one wrote the handoff, then ran
		// out its timeout without restarting the session.
		{"bulleted, as Claude really prints it", "● " + m + "\n", true},
		{"bulleted and indented", "  ●  " + m + "   \n", true},

		// …and the line it must still refuse: the prompt we typed, echoed back
		// into the pane by tmux, wrapped exactly as Claude Code wraps it. Firing
		// on this would kill the session the moment we asked it to save.
		{"wrapped echo of our own prompt", "  including any index or pointer file it needs — print the token " + m + "\n", false},
		{"wrapped echo, continuation", "  " + m + " alone on its own line, as the very last thing you\n", false},
	}
	for _, c := range cases {
		if got := hasMarkerLine(c.pane, m); got != c.want {
			t.Errorf("%s: hasMarkerLine = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWaitQuietStreaming is the regression this whole change exists for: Claude
// prints the marker and keeps working. Quiescence must not be declared while the
// pane is still changing, or the kill-session truncates the memory write we were
// running the checkpoint to preserve.
func TestWaitQuietStreaming(t *testing.T) {
	// Pane changes on every sample: never quiet, must time out.
	n := 0
	capture := func() (string, bool) {
		n++
		return "output line " + time.Duration(n).String(), true
	}
	if waitQuiet(capture, 5*time.Millisecond, 10*time.Millisecond, 120*time.Millisecond) {
		t.Error("declared quiet while the pane was still changing")
	}
	if n < 2 {
		t.Errorf("only sampled %d times; it must poll repeatedly", n)
	}
}

func TestWaitQuietSettles(t *testing.T) {
	// Changes twice, then holds still: must report quiet.
	n := 0
	capture := func() (string, bool) {
		n++
		if n <= 2 {
			return "streaming " + string(rune('a'+n)), true
		}
		return "final", true
	}
	if !waitQuiet(capture, 5*time.Millisecond, 10*time.Millisecond, 2*time.Second) {
		t.Error("pane settled but waitQuiet never said so")
	}
}

func TestWaitQuietCaptureFailure(t *testing.T) {
	if waitQuiet(func() (string, bool) { return "", false }, time.Millisecond, time.Millisecond, time.Second) {
		t.Error("unreadable pane must not count as quiet")
	}
}

func TestWaitForMarker(t *testing.T) {
	const m = "FORGE_CHECKPOINT_SAVED"

	t.Run("appears after a few polls", func(t *testing.T) {
		n := 0
		capture := func() (string, bool) {
			n++
			if n < 3 {
				return "thinking…", true
			}
			return "done\n" + m + "\n", true
		}
		if !waitForMarker(capture, m, 5*time.Millisecond, 2*time.Second) {
			t.Error("marker printed but not detected")
		}
	})

	t.Run("never appears", func(t *testing.T) {
		capture := func() (string, bool) { return "thinking…", true }
		if waitForMarker(capture, m, 5*time.Millisecond, 50*time.Millisecond) {
			t.Error("reported a marker that was never printed")
		}
	})
}

// What Claude writes to the topic file is model output on its way into a shell
// command and a session name, so it gets reduced to one short plain line.
func TestSanitizeTopic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"auth refactor and token expiry", "auth refactor and token expiry"},
		{"", ""},
		{"   \n\n", ""},
		// The first non-empty line wins: a chatty model may add more below.
		{"payment webhook retries\nAlso I updated the tests.\n", "payment webhook retries"},
		{"\n\n  fixing the SSE reconnect  \n", "fixing the SSE reconnect"},
		// Models like to dress a short answer up.
		{`"quota accounting rewrite"`, "quota accounting rewrite"},
		{"**billing edge cases**", "billing edge cases"},
		{"`tmux pane capture`", "tmux pane capture"},
		// Control characters and stray escapes must not reach a name.
		{"deploy\tpipeline\x07 fixes\r", "deploy pipeline fixes"},
		{"a\x1b[31mred\x1b[0m name", "a[31mred[0m name"},
		// Too long: cut at a word boundary, never mid-rune.
		{strings.Repeat("verylongword", 20), strings.Repeat("verylongword", 20)[:maxTopicLen]},
		{"one two three four five six seven eight nine ten eleven twelve thirteen",
			"one two three four five six seven eight nine ten eleven"},
		{"úprava účtování kvót a další věci které jsou opravdu hodně dlouhé a nevejdou se",
			"úprava účtování kvót a další věci které jsou"},
	}
	for _, c := range cases {
		got := sanitizeTopic(c.in)
		if got != c.want {
			t.Errorf("sanitizeTopic(%q) =\n  %q\nwant\n  %q", c.in, got, c.want)
		}
		if len(got) > maxTopicLen {
			t.Errorf("sanitizeTopic(%q) is %d bytes, over the %d cap", c.in, len(got), maxTopicLen)
		}
		if !utf8.ValidString(got) {
			t.Errorf("sanitizeTopic(%q) produced invalid UTF-8: %q", c.in, got)
		}
	}
}
