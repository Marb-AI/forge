package ui

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chatServer is a server wired with a chat that answers from the given stream,
// and a record of what it was asked for.
type chatAsk struct {
	ws     string
	turn   string
	offset int64
	prompt string
}

func chatServer(t *testing.T, stream string, tailErr error) (*server, *chatAsk) {
	t.Helper()
	var got chatAsk
	s := &server{deps: Deps{
		KnowsWorkspace: func(name string) bool { return name == "ws" },
		ChatSend: func(ws, prompt string) (string, error) {
			got.ws, got.prompt = ws, prompt
			return "20260805T142530.123456789", nil
		},
		ChatTail: func(ws, turn string, offset int64, w io.Writer) error {
			got.ws, got.turn, got.offset = ws, turn, offset
			if offset > int64(len(stream)) {
				offset = int64(len(stream))
			}
			_, _ = io.WriteString(w, stream[offset:])
			return tailErr
		},
	}}
	return s, &got
}

const aTurn = "20260805T142530.123456789"

func chatStream(t *testing.T, s *server, url string, header map[string]string) string {
	t.Helper()
	r := httptest.NewRequest("GET", url, nil)
	r.SetPathValue("ws", "ws")
	r.SetPathValue("turn", aTurn)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { defer close(done); s.handleChatStream(w, r) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream handler never returned")
	}
	return w.Body.String()
}

// One line of stream-json is one SSE event, and its id is the byte the reader
// would resume at.
//
// The pairing is the point: stream-json writes one JSON object per line, SSE
// frames on newlines, so the two agree without anything being encoded — and
// unlike the jobs stream beside it, nothing here has to be base64'd on the way
// through and decoded on the way out.
func TestEachLineOfATurnIsOneEventCarryingItsOffset(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","text":"hello"}`,
		`{"type":"result","total_cost_usd":0.01}`,
	}
	stream := strings.Join(lines, "\n") + "\n"
	s, _ := chatServer(t, stream, nil)

	body := chatStream(t, s, "/api/chat/ws/"+aTurn+"/stream", nil)

	var offset int64
	for _, line := range lines {
		offset += int64(len(line)) + 1 // the newline is a byte of the file too
		want := fmt.Sprintf("id: %d\ndata: %s\n\n", offset, line)
		if !strings.Contains(body, want) {
			t.Errorf("the stream does not contain:\n%q\ngot:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "event: done") {
		t.Error("a turn that ended did not say so, so the page cannot tell it from a stall")
	}
}

// A reader that comes back gets the rest, and the browser is what remembers
// where it was: EventSource sends the last id it saw as Last-Event-ID without
// being asked, so a phone out of a tunnel resumes at the byte it stopped on.
func TestAReconnectingReaderIsBelievedOverItsOwnURL(t *testing.T) {
	stream := `{"a":1}` + "\n" + `{"b":2}` + "\n"
	s, got := chatServer(t, stream, nil)

	// The URL a page reconnects with is the one it opened with — offset 0 — while
	// the header carries what it actually received. Trusting the URL would replay
	// the whole turn on every dropped connection.
	body := chatStream(t, s, "/api/chat/ws/"+aTurn+"/stream?offset=0",
		map[string]string{"Last-Event-ID": "8"})

	if got.offset != 8 {
		t.Errorf("resumed at %d, want 8 — the reader was sent what it already had", got.offset)
	}
	if strings.Contains(body, `{"a":1}`) {
		t.Error("the reader was sent a line it had already seen")
	}
	if !strings.Contains(body, `{"b":2}`) {
		t.Error("the reader did not get the line it stopped before")
	}
	// And the ids keep counting from where the reader was, not from this
	// response's first byte.
	if !strings.Contains(body, "id: 16\n") {
		t.Errorf("the resumed ids do not continue the file's offsets:\n%s", body)
	}
}

// An offset from the query is used when there is no header — a first look that
// deliberately skips ahead, and the only path the browser does not drive itself.
func TestAnOffsetInTheQueryIsUsedWhenNothingHasBeenSeenYet(t *testing.T) {
	s, got := chatServer(t, `{"a":1}`+"\n", nil)
	chatStream(t, s, "/api/chat/ws/"+aTurn+"/stream?offset=4", nil)
	if got.offset != 4 {
		t.Errorf("offset %d, want the 4 the query asked for", got.offset)
	}
}

// A turn that stopped being readable is not a turn that finished. The page has
// to be able to tell the difference, or a dropped SSH connection looks exactly
// like Claude having nothing more to say.
func TestATurnThatFailedSaysSoRatherThanEndingQuietly(t *testing.T) {
	s, _ := chatServer(t, `{"a":1}`+"\n", fmt.Errorf("ssh: connection lost"))

	body := chatStream(t, s, "/api/chat/ws/"+aTurn+"/stream", nil)

	if !strings.Contains(body, "event: error") {
		t.Errorf("a failed turn ended like a finished one:\n%s", body)
	}
	if strings.Contains(body, "event: done") {
		t.Error("a failed turn also reported success")
	}
	// SSE frames on newlines, so an error carrying one would end its own event
	// and have the rest read as a field name.
	if !strings.Contains(body, `data: "ssh: connection lost"`) {
		t.Errorf("the reason did not survive the framing:\n%s", body)
	}
}

// Sending is a POST that answers with an id, not with an answer: a request held
// open for the length of a turn is the difference between a chat and a timeout
// on the one connection a phone has.
func TestSendingAnswersWithTheTurnNotTheReply(t *testing.T) {
	s, got := chatServer(t, "", nil)

	r := httptest.NewRequest("POST", "/api/chat/ws/send",
		strings.NewReader(`{"prompt":"what does this do?"}`))
	r.SetPathValue("ws", "ws")
	w := httptest.NewRecorder()
	s.handleChatSend(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("send answered %d: %s", w.Code, w.Body)
	}
	if got.prompt != "what does this do?" {
		t.Errorf("the host was asked %q", got.prompt)
	}
	if !strings.Contains(w.Body.String(), aTurn) {
		t.Errorf("send did not answer with a turn id: %s", w.Body)
	}
}

// An empty prompt is refused here rather than on the host: it is a round trip
// over SSH to be told what this already knows.
func TestAnEmptyPromptIsNotSent(t *testing.T) {
	s, got := chatServer(t, "", nil)

	for _, body := range []string{`{"prompt":""}`, `{"prompt":"   \n "}`, `{}`} {
		r := httptest.NewRequest("POST", "/api/chat/ws/send", strings.NewReader(body))
		r.SetPathValue("ws", "ws")
		w := httptest.NewRecorder()
		s.handleChatSend(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", body, w.Code)
		}
	}
	if got.prompt != "" {
		t.Errorf("an empty prompt reached the host as %q", got.prompt)
	}
}

// A front end wired without chat has no chat, rather than a broken one — the
// same answer every other optional dependency here gives.
func TestAServerWithoutChatSaysSo(t *testing.T) {
	s := &server{deps: Deps{KnowsWorkspace: func(string) bool { return true }}}

	r := httptest.NewRequest("POST", "/api/chat/ws/send", strings.NewReader(`{"prompt":"hi"}`))
	r.SetPathValue("ws", "ws")
	w := httptest.NewRecorder()
	s.handleChatSend(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("send without a chat answered %d, want 501", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/chat/ws/"+aTurn+"/stream", nil)
	r.SetPathValue("ws", "ws")
	r.SetPathValue("turn", aTurn)
	w = httptest.NewRecorder()
	s.handleChatStream(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("streaming without a chat answered %d, want 501", w.Code)
	}
}

// A workspace this client did not create is a 404 before anything is attempted,
// like every other per-workspace endpoint here.
func TestChatRefusesAnUnknownWorkspace(t *testing.T) {
	s, got := chatServer(t, "", nil)

	r := httptest.NewRequest("POST", "/api/chat/other/send", strings.NewReader(`{"prompt":"hi"}`))
	r.SetPathValue("ws", "other")
	w := httptest.NewRecorder()
	s.handleChatSend(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown workspace answered %d, want 404", w.Code)
	}
	if got.ws != "" {
		t.Errorf("it was sent to the host anyway, as %q", got.ws)
	}
}

// An offset that is not a number, or is before the start, is refused rather than
// passed on as a zero — the difference is a turn silently replayed from the top.
func TestABadOffsetIsRefused(t *testing.T) {
	s, got := chatServer(t, "", nil)

	for _, bad := range []string{"-1", "abc", "9999999999999999999999"} {
		r := httptest.NewRequest("GET", "/api/chat/ws/"+aTurn+"/stream?offset="+bad, nil)
		r.SetPathValue("ws", "ws")
		r.SetPathValue("turn", aTurn)
		w := httptest.NewRecorder()
		s.handleChatStream(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("offset %q answered %d, want 400", bad, w.Code)
		}
	}
	if got.ws != "" {
		t.Error("a bad offset reached the host")
	}
}

// A line past the limit ends the turn, and does not hang it.
//
// The framing reads the tail through a pipe, so a scanner that gives up stops
// reading it — and the tail on the other end blocks on a write nobody will ever
// take, holding an SSH session open until the browser goes away. The reader is
// closed with whatever stopped the framing, which is what unwinds it: the tail's
// write returns an error instead of never returning at all.
func TestALinePastTheLimitEndsTheTurnRatherThanHangingIt(t *testing.T) {
	huge := "{\"text\":\"" + strings.Repeat("x", chatLineLimit+1) + "\"}\n"

	wrote := make(chan error, 1)
	s := &server{deps: Deps{
		KnowsWorkspace: func(string) bool { return true },
		ChatTail: func(ws, turn string, offset int64, w io.Writer) error {
			_, err := io.WriteString(w, huge)
			wrote <- err
			return err
		},
	}}

	// chatStream fails the test if the handler never returns, which is what this
	// hangs as without the close.
	body := chatStream(t, s, "/api/chat/ws/"+aTurn+"/stream", nil)

	if err := <-wrote; err == nil {
		t.Error("the tail's write was accepted whole, so this proves nothing about " +
			"the line the framing could not carry")
	}
	if !strings.Contains(body, "event: error") {
		t.Errorf("a turn the page could not frame ended as though it had finished:\n%s",
			firstBytes(body))
	}
	if strings.Contains(body, "event: done") {
		t.Error("it also reported success")
	}
}

func firstBytes(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
