package ui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Saved prompts: the texts you send Claude over and over, kept behind one rail
// button rather than in Settings. They are not settings — nothing about them
// changes how Forge behaves — they are content, and content you edit often.
//
// They live in the client's config (~/.forge/config.json), which makes them the
// same list in every workspace, on every host, and in every browser pointed at
// this daemon. That is deliberate: a prompt describes how its author works, not
// what a particular codebase needs.
//
// The daemon only stores them. Sending one is the browser's job — it types the
// text into the Claude terminal over the same input endpoint as your keyboard —
// so there is no endpoint here that can make Claude do anything.

const (
	// A title has to stay readable in a narrow popover row; the text is a prompt,
	// which can reasonably be a couple of paragraphs.
	maxPromptTitle = 80
	maxPromptText  = 16 << 10
	// A ceiling so a stuck script can't grow the config without bound. Far more
	// than a list you scroll by hand would ever hold.
	maxPrompts = 200
)

// handlePromptsList returns the saved prompts in order.
func (s *server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	// Edited from other tabs, so a cached copy would resurrect a deleted prompt.
	w.Header().Set("Cache-Control", "no-store")
	list, err := s.deps.Prompts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, orEmpty(list))
}

// handlePromptCreate appends a prompt and returns it — with the id the browser
// needs to edit or delete it later.
func (s *server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	title, text, err := decodePrompt(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	id, err := newPromptID()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	// Read-modify-write under the lock: two tabs saving at once would otherwise
	// each load the same list, append to it, and the second save would drop the
	// first one's prompt.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	list, err := s.deps.Prompts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if len(list) >= maxPrompts {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("that's %d prompts already — delete one first", maxPrompts))
		return
	}
	p := Prompt{ID: id, Title: title, Text: text}
	if err := s.deps.SetPrompts(append(list, p)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, p)
}

// handlePromptUpdate rewrites one prompt in place, keeping its position in the
// list — editing a prompt should not move it out from under the cursor.
func (s *server) handlePromptUpdate(w http.ResponseWriter, r *http.Request) {
	title, text, err := decodePrompt(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")

	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	list, err := s.deps.Prompts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	i := indexOfPrompt(list, id)
	if i < 0 {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("no such prompt"))
		return
	}
	list[i].Title, list[i].Text = title, text
	if err := s.deps.SetPrompts(list); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, list[i])
}

// handlePromptDelete forgets a prompt. No confirmation here — the browser asks
// first, and unlike a workspace this destroys nothing but a few lines of text.
func (s *server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	list, err := s.deps.Prompts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	i := indexOfPrompt(list, id)
	if i < 0 {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("no such prompt"))
		return
	}
	if err := s.deps.SetPrompts(append(list[:i:i], list[i+1:]...)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// decodePrompt reads and validates the {title, text} body both writes share.
func decodePrompt(r *http.Request) (title, text string, err error) {
	var req struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	// The limit is on the wire, so it can reject a huge body without buffering it;
	// the per-field checks below are what the user actually sees.
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*maxPromptText)).Decode(&req); err != nil {
		return "", "", fmt.Errorf("bad request")
	}
	// The text is stored VERBATIM. It is going to be typed into a session, so its
	// whitespace is content: a prompt that opens with an indented line, or with a
	// blank one, should arrive that way. Trimming is only how we decide it is
	// empty. The title has no such claim on it — it is a label in a list.
	title, text = strings.TrimSpace(req.Title), req.Text
	if title == "" {
		return "", "", fmt.Errorf("give it a title — that's how you'll find it in the list")
	}
	if strings.TrimSpace(text) == "" {
		return "", "", fmt.Errorf("a prompt with no text would send nothing")
	}
	if utf8.RuneCountInString(title) > maxPromptTitle {
		return "", "", fmt.Errorf("title is too long (max %d characters)", maxPromptTitle)
	}
	if len(text) > maxPromptText {
		return "", "", fmt.Errorf("prompt text is too long (max %d KB)", maxPromptText>>10)
	}
	// A title is one line in a list; a newline in it would break the row rather
	// than say anything. The text keeps every line it has.
	title = strings.Join(strings.Fields(title), " ")
	return title, text, nil
}

func indexOfPrompt(list []Prompt, id string) int {
	if id == "" {
		return -1
	}
	for i, p := range list {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// newPromptID mints an opaque id. Not the title: titles are edited, duplicated
// and non-ASCII, and every one of those would break a URL that identified a
// prompt by it.
func newPromptID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot generate an id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// orEmpty makes a nil list marshal as [] rather than null — the browser iterates
// what it gets.
func orEmpty(list []Prompt) []Prompt {
	if list == nil {
		return []Prompt{}
	}
	return list
}
