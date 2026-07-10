package cli

import (
	"testing"
	"time"
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
