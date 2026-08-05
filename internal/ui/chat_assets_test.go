package ui

import (
	"os"
	"strings"
	"testing"
)

// The chat sits under the shell panel, and both sit under the modals.
//
// It is not decoration: chat and the terminal are two faces of one session, and
// a shell is something you open *beside* whichever face you are looking at. A
// chat that covered the shell would make the ssh button do nothing while a
// conversation was open, which is exactly the kind of dead control nobody
// reports because it looks deliberate.
func TestTheChatSitsUnderTheShellAndTheModals(t *testing.T) {
	css := embeddedAsset(t, "app.css")

	chat := zIndexOf(t, css, "#chatpanel")
	shell := zIndexOf(t, css, "#sshpanel")
	modal := zIndexOf(t, css, "#confirm")

	if !(chat < shell) {
		t.Errorf("#chatpanel is at %d and #sshpanel at %d — a shell could not open "+
			"over a chat", chat, shell)
	}
	if !(shell < modal) {
		t.Errorf("#sshpanel is at %d and #confirm at %d", shell, modal)
	}
}

// Enter sends, Shift+Enter breaks the line. Every chat works this way, so
// fingers arrive already knowing it, and the one that does not is the one that
// sends half a paragraph.
func TestEnterSendsAndShiftEnterDoesNot(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "initChat")

	if !strings.Contains(body, "e.key === \"Enter\"") {
		t.Fatal("the composer does not treat Enter specially")
	}
	if !strings.Contains(body, "!e.shiftKey") {
		t.Error("Shift+Enter sends, so a paragraph cannot be written")
	}
	// A composition — an IME mid-word — ends on Enter, and that Enter is not a
	// send. Anyone typing Japanese would otherwise send every word.
	if !strings.Contains(body, "isComposing") {
		t.Error("Enter sends mid-composition, which breaks every IME")
	}
}

// The offset lives in SSE ids and nowhere else.
//
// That is the whole reason the server puts it there: EventSource remembers the
// last id it saw and sends it back as Last-Event-ID on its own reconnect, so a
// laptop that slept resumes at the byte it stopped on with nothing in the page
// remembering anything. A client that also passed an offset would fight its own
// browser — and lose, since the header is believed over the query.
func TestTheChatDoesNotTrackItsOwnOffset(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "chatOpenStream")

	if !strings.Contains(body, "new EventSource(") {
		t.Fatal("the chat does not stream with EventSource, so nothing carries the offset")
	}
	// The comments explain why there is no offset here; it is the code that must
	// not have one.
	if code := withoutComments(body); strings.Contains(code, "offset") {
		t.Errorf("chatOpenStream tracks an offset; the browser carries it:\n%s", code)
	}
	// And it must not treat a transport error as the turn's: the browser is
	// reconnecting, and saying so would put a failure on screen for every tunnel.
	if !strings.Contains(body, "es.onerror = () => {}") {
		t.Error("a transient disconnect is reported to the reader as a failure")
	}
}

// The server's failure event must not be called "error".
//
// EventSource dispatches its own transport trouble as an `error` event, so a
// listener by that name hears every dropped connection as well as the one real
// failure — and this page ends the turn on it, which closes the stream and stops
// the browser reconnecting. That is the entire mechanism the ids exist for: the
// first tunnel a phone drives through would end the conversation and report it
// as Claude having stopped being readable.
//
// Both ends are checked here because they only work as a pair.
func TestTheServersFailureIsNotCalledError(t *testing.T) {
	js := withoutComments(embeddedAsset(t, "app.js"))

	if strings.Contains(js, `es.addEventListener("error"`) {
		t.Error("the chat listens for `error`, which is also every dropped connection: " +
			"a tunnel would end the turn and stop the browser reconnecting")
	}
	if !strings.Contains(js, `es.addEventListener("failed"`) {
		t.Fatal("nothing listens for the server's failure, so a turn that broke ends silently")
	}

	// And the server has to send that name, or the listener is deaf.
	server, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(server), `"event: failed\ndata:`) {
		t.Error("the handler does not send `event: failed`, which is what the page waits for")
	}
}

// withoutComments strips // lines, so a guard reads the code and not the prose
// explaining it — which in this file says the very words being searched for.
func withoutComments(js string) string {
	var out []string
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// A reader who scrolled up is reading. Yanking the view back down is the one
// thing a log must not do, and it is the thing every naive one does.
func TestTheChatOnlyFollowsWhenItWasAlreadyAtTheBottom(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "chatAppend")

	if !strings.Contains(body, "scrollHeight") || !strings.Contains(body, "clientHeight") {
		t.Fatal("chatAppend does not look at where the reader is")
	}
	if !strings.Contains(body, "atBottom") {
		t.Error("chatAppend scrolls unconditionally, so reading anything older is impossible")
	}
}

// Partial text is a preview of the finished message, so exactly one of them may
// be on screen. Rendering both is the bug this format invites: everything Claude
// says would appear twice, once as it was typed and once again after.
func TestStreamedTextIsReplacedByTheFinishedMessage(t *testing.T) {
	body := jsFunc(t, embeddedAsset(t, "app.js"), "chatRender")

	i := strings.Index(body, `type === "assistant"`)
	if i < 0 {
		t.Fatal("chatRender does not handle finished assistant messages")
	}
	rest := body[i:]
	if !strings.Contains(rest, "c.live.remove()") {
		t.Error("the finished message is appended without removing what streamed, " +
			"so every reply would be shown twice")
	}
}

// The panel has to be reachable, and the button has to say which of the two
// faces is showing.
func TestTheChatButtonIsWiredBothWays(t *testing.T) {
	index := embeddedAsset(t, "index.html")
	js := embeddedAsset(t, "app.js")

	if !strings.Contains(index, `data-action="chat"`) {
		t.Error("there is no way into the chat")
	}
	if !strings.Contains(index, `id="chatpanel"`) {
		t.Error("index.html has no chat panel")
	}
	// setPanelActive lights the rail for whatever owns the stage; chat is not a
	// shell, so it needs saying explicitly or the button stays dark while its
	// panel is open.
	if !strings.Contains(jsFunc(t, js, "setPanelActive"), `data-action="chat"`) {
		t.Error("the rail does not light up for the chat")
	}
}
