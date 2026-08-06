package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// guarded is the guard in front of a handler that records whether it was ever
// reached — which is the whole question here, since a refused request and an
// allowed one both come back opaque to the page that made it.
func guarded(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	reached := false
	s := &server{token: "t0ken"}
	return s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})), &reached
}

func ask(t *testing.T, h http.Handler, method, path string, headers map[string]string) int {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Host = "127.0.0.1:8080"
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "t0ken"})
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// A page on a port Forge itself tunnels must not be able to make Forge do
// anything — including the things that are GETs.
//
// It is not a hypothetical origin: it is a dev server in a workspace, running
// code Claude wrote, on a port this client opened a tunnel to and put a link to
// on screen. It cannot read the token (the cookie is HttpOnly) and it cannot
// read a response (no CORS headers, so the answer is opaque). But the browser
// attaches the cookie to its request anyway — SameSite is about *sites*, and a
// different port on 127.0.0.1 is the same site — so the request runs.
//
// Which matters because several of these GETs are not reads. Opening a terminal
// is a GET. One of them opens a shell on this machine.
func TestAPageOnATunnelledPortCannotOpenTerminals(t *testing.T) {
	h, reached := guarded(t)

	for _, path := range []string{
		"/api/term/ws/claude/stream",
		"/api/term/local/stream", // a shell on the machine you are sitting at
		"/api/workspaces",
	} {
		*reached = false
		code := ask(t, h, "GET", path, map[string]string{
			// What a browser sends for 127.0.0.1:16042 → 127.0.0.1:8080. Same site,
			// different origin, and the difference is the point.
			"Sec-Fetch-Site": "same-site",
			"Origin":         "http://127.0.0.1:16042",
		})
		if code != http.StatusForbidden {
			t.Errorf("GET %s from a tunnelled port answered %d, want 403", path, code)
		}
		if *reached {
			t.Errorf("GET %s ran — a page on a workspace's own port made Forge act", path)
		}
	}
}

// And the page itself keeps working, which is the other half: a guard that
// refuses the UI is not a guard.
func TestThePagesOwnRequestsStillWork(t *testing.T) {
	h, reached := guarded(t)

	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "same-origin"},                                    // fetch and EventSource from the page
		{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8080"}, // and with an Origin
		{"Sec-Fetch-Site": "none"},                                           // typed, or a bookmark
		{},                                                                   // curl, or a browser too old to say
	} {
		*reached = false
		if code := ask(t, h, "GET", "/api/workspaces", headers); code != http.StatusOK {
			t.Errorf("%v answered %d, want the page to work", headers, code)
		}
		if !*reached {
			t.Errorf("%v never reached the handler", headers)
		}
	}
}

// Opening the UI is a navigation: no Origin, no Sec-Fetch-Site worth the name,
// and it must not be caught by a rule aimed at the API.
func TestOpeningTheUIIsNotAnAPICall(t *testing.T) {
	h, reached := guarded(t)

	*reached = false
	code := ask(t, h, "GET", "/", map[string]string{"Sec-Fetch-Site": "none"})
	if code != http.StatusOK || !*reached {
		t.Errorf("opening the UI answered %d (reached=%v)", code, *reached)
	}
	// And an asset fetched by that page, which is same-origin anyway.
	*reached = false
	if code := ask(t, h, "GET", "/assets/app.js",
		map[string]string{"Sec-Fetch-Site": "same-origin"}); code != http.StatusOK {
		t.Errorf("an asset answered %d", code)
	}
}

// Cross-site is refused for the same reason same-site is, and was already.
func TestSomethingElseEntirelyIsStillRefused(t *testing.T) {
	h, reached := guarded(t)

	*reached = false
	code := ask(t, h, "GET", "/api/workspaces", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "https://example.com",
	})
	if code != http.StatusForbidden || *reached {
		t.Errorf("a cross-site GET answered %d (reached=%v)", code, *reached)
	}
}
