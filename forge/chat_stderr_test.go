package forge

import (
	"errors"
	"strings"
	"testing"
)

// The sentence that explains a failed tail is on stderr, and it has to survive.
//
// The agent's tail command carries Claude Code's stream on stdout, so it says
// everything it has to say on stderr — "no turn X in this workspace" among it.
// Left to this process's stderr that lands in a daemon log, while the person
// waiting for an answer gets whatever the transport called the exit status. It
// is the one sentence worth putting in front of them.
func TestWhyATailFailedReachesWhoeverAsked(t *testing.T) {
	transport := errors.New("ssh: process exited with status 1")

	var said boundedBuffer
	_, _ = said.Write([]byte("forge-agent: no turn 20260805T142530.123456789 in \"ws\"\n"))

	// The far end's own words, when it had any.
	if got := explain(transport, &said); !strings.Contains(got.Error(), "no turn") {
		t.Errorf("the browser would be told %q, which nobody can act on", got)
	}
	// And the transport's, when it did not — a connection that never arrived has
	// nothing to say for itself.
	var silent boundedBuffer
	if got := explain(transport, &silent); !errors.Is(got, transport) {
		t.Errorf("a silent failure was reported as %q instead of what went wrong", got)
	}
	// A success stays a success, however talkative: a warning on stderr is not a
	// failure, and turning one into an error would end a turn that went fine.
	if got := explain(nil, &said); got != nil {
		t.Errorf("a turn that finished was reported as failing: %v", got)
	}
}

// What it holds is destined for a browser, and the far end is a remote process
// that can be made to say anything at any length.
func TestAnExplanationIsBoundedButNotShortened(t *testing.T) {
	var why boundedBuffer

	// Written the way a pipe writes: many times, in chunks the far end chose.
	const chunk = 512
	for i := 0; i < 100; i++ {
		n, err := why.Write([]byte(strings.Repeat("x", chunk)))
		if err != nil {
			t.Fatalf("the buffer refused a write: %v", err)
		}
		// A short write ends the stream it belongs to, and there is nothing to
		// resend: the far end is not being asked to hold anything back, only to be
		// ignored past a point.
		if n != chunk {
			t.Fatalf("reported %d of %d bytes taken, which would end the stream", n, chunk)
		}
	}
	if why.Len() != chatWhyLimit {
		t.Errorf("kept %d bytes, want it capped at %d", why.Len(), chatWhyLimit)
	}

	// And the first of it is what is kept: an explanation reads from the top, and
	// the useful sentence is the one the agent wrote before whatever followed.
	var short boundedBuffer
	_, _ = short.Write([]byte("the reason\n"))
	_, _ = short.Write([]byte(strings.Repeat("noise\n", 10000)))
	if !strings.HasPrefix(short.String(), "the reason\n") {
		t.Errorf("the beginning was dropped: %q", firstOf(short.String()))
	}
}

func firstOf(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
