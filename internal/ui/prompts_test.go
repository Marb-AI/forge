package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// promptServer is a UI server whose saved prompts live in memory, so the
// handlers can be exercised without touching the real ~/.forge/config.json.
func promptServer(t *testing.T) (*server, http.Handler, func() []Prompt) {
	t.Helper()
	s, h := testServer(t)
	var mu sync.Mutex
	var store []Prompt
	s.deps.Prompts = func() ([]Prompt, error) {
		mu.Lock()
		defer mu.Unlock()
		// A copy, like a real load from disk: a handler that mutated the store in
		// place would pass this test while corrupting the file it never wrote.
		return append([]Prompt(nil), store...), nil
	}
	s.deps.SetPrompts = func(list []Prompt) error {
		mu.Lock()
		defer mu.Unlock()
		store = append([]Prompt(nil), list...)
		return nil
	}
	return s, h, func() []Prompt {
		mu.Lock()
		defer mu.Unlock()
		return append([]Prompt(nil), store...)
	}
}

// authorizedDo issues a cookie-carrying request of any method (no Origin, which
// sameOrigin treats as same-origin — see the guard tests).
func authorizedDo(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:47615"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createPrompt adds one and returns it as the API reported it.
func createPrompt(t *testing.T, h http.Handler, title, text string) Prompt {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"title": title, "text": text})
	rec := authorizedDo(h, "POST", "/api/prompts", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q: status %d (%s)", title, rec.Code, rec.Body)
	}
	var p Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("create %q: bad JSON: %v (%s)", title, err, rec.Body)
	}
	return p
}

func listPromptsAPI(t *testing.T, h http.Handler) []Prompt {
	t.Helper()
	rec := authorized(h, "/api/prompts")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d (%s)", rec.Code, rec.Body)
	}
	var got []Prompt
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("list: bad JSON: %v (%s)", err, rec.Body)
	}
	return got
}

func TestPromptsRoundTripThroughTheAPI(t *testing.T) {
	_, h, store := promptServer(t)

	first := createPrompt(t, h, "review the diff", "Review the diff and tell me what you'd change.")
	second := createPrompt(t, h, "run the tests", "Run the tests and fix whatever breaks.")
	third := createPrompt(t, h, "commit", "Commit what's staged with a message in the repo's style.")

	// The list is the user's order, and it is the order they were added in.
	got := listPromptsAPI(t, h)
	if len(got) != 3 {
		t.Fatalf("expected 3 prompts, got %d: %+v", len(got), got)
	}
	for i, want := range []string{"review the diff", "run the tests", "commit"} {
		if got[i].Title != want {
			t.Errorf("prompt %d = %q, want %q", i, got[i].Title, want)
		}
	}

	// An edit rewrites in place. Moving it would shuffle the list under a cursor
	// that is about to click the row below.
	body, _ := json.Marshal(map[string]string{"title": "run the tests", "text": "Run go test ./... and fix what breaks."})
	if rec := authorizedDo(h, "PUT", "/api/prompts/"+second.ID, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("update: status %d (%s)", rec.Code, rec.Body)
	}
	got = listPromptsAPI(t, h)
	if got[1].ID != second.ID || got[1].Text != "Run go test ./... and fix what breaks." {
		t.Errorf("update did not rewrite in place: %+v", got)
	}

	// Delete takes exactly one, and leaves the rest in order.
	if rec := authorizedDo(h, "DELETE", "/api/prompts/"+second.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d (%s)", rec.Code, rec.Body)
	}
	got = listPromptsAPI(t, h)
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != third.ID {
		t.Errorf("after delete the list is %+v, want %q then %q", got, first.ID, third.ID)
	}
	if persisted := store(); len(persisted) != 2 {
		t.Errorf("the store holds %d prompts, want 2 — the list must be saved, not just answered", len(persisted))
	}
}

// Every prompt needs an id of its own: the browser addresses one by id to edit
// or delete it, so a collision would edit the wrong prompt.
func TestEachPromptGetsItsOwnID(t *testing.T) {
	_, h, _ := promptServer(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		// Same title deliberately — ids must not be derived from it.
		p := createPrompt(t, h, "same title", "text")
		if p.ID == "" {
			t.Fatal("a created prompt came back with no id")
		}
		if seen[p.ID] {
			t.Fatalf("duplicate prompt id %q", p.ID)
		}
		seen[p.ID] = true
	}
}

func TestPromptRejectsEmptyTitleOrText(t *testing.T) {
	_, h, store := promptServer(t)

	cases := map[string]string{
		"no title":        `{"title":"","text":"something"}`,
		"blank title":     `{"title":"   ","text":"something"}`,
		"no text":         `{"title":"a title","text":""}`,
		"whitespace text": `{"title":"a title","text":"\n\t "}`,
		"junk body":       `not json`,
	}
	for name, body := range cases {
		if rec := authorizedDo(h, "POST", "/api/prompts", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body)
		}
	}
	if len(store()) != 0 {
		t.Errorf("a rejected prompt must not be saved, store holds %+v", store())
	}
}

// A title has to fit a narrow row, and the text is the thing that gets typed
// into a session — both are bounded, and the message must say which one it is.
func TestPromptLengthLimits(t *testing.T) {
	_, h, store := promptServer(t)

	long := strings.Repeat("x", maxPromptTitle+1)
	body, _ := json.Marshal(map[string]string{"title": long, "text": "fine"})
	rec := authorizedDo(h, "POST", "/api/prompts", string(body))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "title") {
		t.Errorf("an over-long title should be a 400 naming the title, got %d (%s)", rec.Code, rec.Body)
	}

	body, _ = json.Marshal(map[string]string{"title": "fine", "text": strings.Repeat("x", maxPromptText+1)})
	rec = authorizedDo(h, "POST", "/api/prompts", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an over-long text should be a 400, got %d (%s)", rec.Code, rec.Body)
	}
	if len(store()) != 0 {
		t.Errorf("neither should have been saved, store holds %+v", store())
	}

	// The maximum itself is allowed — an off-by-one here would reject a prompt the
	// UI's own counter called fine.
	createPrompt(t, h, strings.Repeat("x", maxPromptTitle), strings.Repeat("y", maxPromptText))
}

// A title is one line in a list. A newline in it would break the row rather than
// say anything, so it is folded away — while the text keeps every line it has.
func TestPromptTitleIsFlattenedButTextIsNot(t *testing.T) {
	_, h, _ := promptServer(t)
	p := createPrompt(t, h, "two\nlines", "keep\nboth\nlines")
	if strings.Contains(p.Title, "\n") {
		t.Errorf("title kept its newline: %q", p.Title)
	}
	if p.Text != "keep\nboth\nlines" {
		t.Errorf("text = %q, want its newlines kept", p.Text)
	}
}

// The text is typed into a session, so its whitespace is content: an opening
// indent or a deliberate blank first line is part of the prompt, and storing a
// trimmed version would send something the author never wrote.
func TestPromptTextIsStoredVerbatim(t *testing.T) {
	_, h, _ := promptServer(t)
	const verbatim = "\n    indented first line\nand a second\n\n"
	p := createPrompt(t, h, "keeps its shape", verbatim)
	if p.Text != verbatim {
		t.Errorf("text = %q, want it stored exactly as written (%q)", p.Text, verbatim)
	}
	// It has to survive the list too, not just the create response.
	if got := listPromptsAPI(t, h); got[0].Text != verbatim {
		t.Errorf("listed text = %q, want %q", got[0].Text, verbatim)
	}
	// Trimming is still how emptiness is decided — whitespace alone is no prompt.
	if rec := authorizedDo(h, "POST", "/api/prompts", `{"title":"t","text":"  \n\t "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("whitespace-only text = %d, want 400", rec.Code)
	}
}

func TestPromptUnknownIDIs404(t *testing.T) {
	_, h, _ := promptServer(t)
	body := `{"title":"t","text":"x"}`
	if rec := authorizedDo(h, "PUT", "/api/prompts/nope", body); rec.Code != http.StatusNotFound {
		t.Errorf("PUT to an unknown id = %d, want 404", rec.Code)
	}
	if rec := authorizedDo(h, "DELETE", "/api/prompts/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown id = %d, want 404", rec.Code)
	}
}

// The browser iterates what it gets, so an empty library must be [] and not the
// null that a nil slice marshals to.
func TestEmptyPromptListIsAnEmptyArray(t *testing.T) {
	_, h, _ := promptServer(t)
	rec := authorized(h, "/api/prompts")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty list rendered as %s, want []", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — another tab may have just edited the list", cc)
	}
}

// The store is load-modify-save, so two tabs saving at the same moment would
// each read the same list, append to it, and the second save would drop the
// first one's prompt. The lock is what makes every one of them survive.
func TestConcurrentPromptWritesDoNotLoseAny(t *testing.T) {
	_, h, store := promptServer(t)

	const writers = 12
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"title": "p", "text": "text"})
			if rec := authorizedDo(h, "POST", "/api/prompts", string(body)); rec.Code != http.StatusCreated {
				t.Errorf("concurrent create: status %d (%s)", rec.Code, rec.Body)
			}
		}(i)
	}
	wg.Wait()

	if got := len(store()); got != writers {
		t.Errorf("%d concurrent creates left %d prompts — writes are being lost", writers, got)
	}
}

func TestPromptListCannotGrowWithoutBound(t *testing.T) {
	s, h, _ := promptServer(t)
	full := make([]Prompt, maxPrompts)
	for i := range full {
		full[i] = Prompt{ID: string(rune('a' + i%26)), Title: "t", Text: "x"}
	}
	_ = s.deps.SetPrompts(full)

	body := `{"title":"one more","text":"x"}`
	if rec := authorizedDo(h, "POST", "/api/prompts", body); rec.Code != http.StatusBadRequest {
		t.Errorf("creating past the cap = %d, want 400", rec.Code)
	}
}
