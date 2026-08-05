package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Chat over HTTP.
//
// Two endpoints, because a turn is asked once and read as often as anybody
// likes: a POST that starts one and answers with its id, and a stream that
// replays it from wherever the reader got to.
//
// The stream is SSE, like the terminals and the jobs before it — which is not
// only consistency. A desktop or phone shell serves this page from Go memory
// through a custom scheme if it is allowed to, and that path hands a request its
// whole body at once; loopback and SSE is what actually streams (see
// cmd/forge-app). A chat that arrived when the turn ended would be a form
// submission with extra steps.

// errNoChat is a front end wired without chat — answered rather than crashed,
// the way every other optional dependency here is, so a caller that has not got
// round to it has no chat instead of a broken one.
var errNoChat = errors.New("chat is not available")

// handleChatSend starts a turn and answers with the id to read it back by.
func (s *server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if s.deps.ChatSend == nil {
		writeJSONError(w, http.StatusNotImplemented, errNoChat)
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, chatPromptLimit)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unreadable request"))
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("nothing to send"))
		return
	}
	turn, err := s.deps.ChatSend(ws, body.Prompt)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{"turn": turn})
}

// chatPromptLimit bounds what one prompt may be. Generous — a person pasting a
// stack trace and three files is the ordinary case, not the abusive one — but
// not unbounded, because this is a body from a browser and the far end writes it
// to a file on somebody's server.
const chatPromptLimit = 1 << 20 // 1 MiB

// handleChatHistory replays the conversation so far.
//
// The same framing as a live turn, with one difference: no ids. An id is a byte
// offset into one turn's file, and this is several concatenated, so a number
// from here handed back as Last-Event-ID would resume a turn at a place that
// means nothing in it. History is finite and the page asks for it once, so there
// is nothing to resume.
func (s *server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if s.deps.ChatHistory == nil {
		writeJSONError(w, http.StatusNotImplemented, errNoChat)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	turns := chatTurns(r)

	sseHeaders(w)
	flusher.Flush()

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.deps.ChatHistory(ws, turns, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	go func() {
		<-r.Context().Done()
		_ = pr.CloseWithError(r.Context().Err())
	}()

	readErr := streamChat(w, flusher, pr, noIDs)
	_ = pr.CloseWithError(readErr)
	if err := <-done; readErr == nil {
		readErr = err
	}
	chatEnd(w, flusher, readErr, r)
}

// chatTurns is how much of the conversation to bring back. Bounded, because the
// number arrives from a browser and each one is a file read on somebody's
// server; defaulted, because a page that asks for none wants the usual amount.
func chatTurns(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("turns"))
	if err != nil || n <= 0 {
		return chatTurnsDefault
	}
	if n > chatTurnsMax {
		return chatTurnsMax
	}
	return n
}

const (
	chatTurnsDefault = 20
	chatTurnsMax     = 200
)

// noIDs is the offset that means "do not number these events" — see
// handleChatHistory for why several turns at once cannot be resumed by offset.
const noIDs int64 = -1

// handleChatStream replays a turn as it was written, and keeps going until it
// ends.
//
// Every event carries the byte offset that follows it as its SSE id, which is
// the whole of the reconnection story: EventSource remembers the last id it saw
// and sends it back as Last-Event-ID without being asked, so a phone that spent
// twenty minutes in a tunnel resumes at the byte it stopped on, with nothing
// repeated and nothing missed. The client keeps no bookkeeping at all.
func (s *server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	if s.deps.ChatTail == nil {
		writeJSONError(w, http.StatusNotImplemented, errNoChat)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	offset, err := chatOffset(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	sseHeaders(w)
	flusher.Flush()

	// The turn's bytes go through a pipe rather than a buffer, so the tail's own
	// pace is the browser's: a paragraph reaches the page when the host wrote it.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.deps.ChatTail(ws, r.PathValue("turn"), offset, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	// A browser that closes the tab must not leave a turn being tailed over SSH
	// for as long as it runs: closing the read end is what unwinds the writer.
	go func() {
		<-r.Context().Done()
		_ = pr.CloseWithError(r.Context().Err())
	}()

	// The reader is closed with whatever stopped the framing, and always: a
	// scanner that gave up — a line past the limit — stops reading the pipe, and
	// the tail on the other end would then block on a write nobody will ever take,
	// holding an SSH session open until the browser went away. Closing is what
	// unwinds it.
	readErr := streamChat(w, flusher, pr, offset)
	_ = pr.CloseWithError(readErr)

	tailErr := <-done
	// The framing's own failure is the more specific of the two — the tail will
	// only report the broken pipe that this caused — so it is the one to show.
	if readErr == nil {
		readErr = tailErr
	}
	chatEnd(w, flusher, readErr, r)
}

// sseHeaders says what every stream here says. One place, because a stream that
// disagreed about buffering would work locally and stall behind a proxy.
func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// chatEnd closes a stream by saying which way it ended. The page can then tell
// "finished" from "stopped being readable", which it cannot otherwise: both are
// a stream that stops.
//
// "failed", not "error": EventSource dispatches its own transport trouble as an
// `error` event, so a server event by that name arrives through the same
// listener as every dropped connection — and a page that treated one as the
// other would end the turn on the first tunnel and stop the browser's
// reconnect, which is the whole mechanism the ids exist for.
//
// A client that went away is told nothing, because there is nobody there.
func chatEnd(w io.Writer, flusher http.Flusher, err error, r *http.Request) {
	if err != nil && r.Context().Err() == nil {
		fmt.Fprintf(w, "event: failed\ndata: %s\n\n", sseData(err.Error()))
	} else {
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	}
	flusher.Flush()
}

// streamChat turns the turn's NDJSON into SSE events, one per line.
//
// A line at a time because that is what the format is: stream-json writes one
// JSON object per line, SSE frames on newlines, and the two agree without
// anything being encoded. Passing chunks through instead would split objects
// across events at whatever boundary the network chose.
func streamChat(w io.Writer, flusher http.Flusher, r io.Reader, offset int64) error {
	sc := bufio.NewScanner(r)
	// A turn's line is one JSON object and can hold a whole file the model wrote
	// out; the default 64 KiB would end the stream partway through one.
	sc.Buffer(make([]byte, 0, 64*1024), chatLineLimit)

	for sc.Scan() {
		line := sc.Bytes()
		// The scanner drops the newline, and the offset has to count it: it is a
		// byte of the file, and a resume that forgot it would start mid-line.
		offset += int64(len(line)) + 1
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", offset, line)
		flusher.Flush()
	}
	return sc.Err()
}

// chatLineLimit is the longest single stream-json object this will carry. One
// line can contain a file Claude just wrote, so the bound is what a page could
// plausibly want rather than what a message usually is.
const chatLineLimit = 8 << 20 // 8 MiB

// chatOffset is where this reader left off: the browser's own Last-Event-ID if
// it is reconnecting, and the query otherwise.
//
// The header wins because it is the more recent fact. A page that reconnects
// keeps the URL it opened with — offset=0, most of the time — while the header
// carries what it actually received, and trusting the URL over it would replay
// the turn from the top on every dropped connection.
func chatOffset(r *http.Request) (int64, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("offset")
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad offset %q", raw)
	}
	return n, nil
}

// sseData makes a string safe to send as one event's data: SSE frames on
// newlines, so a message carrying one would end its own event and have the rest
// read as a new field.
func sseData(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"unreportable"`
	}
	return string(b)
}
