package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Marb-AI/forge/internal/config"
)

// testServer builds a server with the guard in place and a trivial handler
// behind it, so the security model can be exercised end to end in-process.
func testServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	s := &server{
		token:     "secret-token",
		terms:     newTermRegistry(),
		ckRunning: map[string]bool{},
		jobs:      map[string]*job{},
		deps: Deps{
			ListWorkspaces:  func() ([]WorkspaceInfo, error) { return []WorkspaceInfo{}, nil },
			HostFor:         func(string) *config.Host { return nil },
			Checkpoint:      func(string, io.Writer) error { return nil },
			ListHosts:       func() ([]string, error) { return []string{}, nil },
			CreateWorkspace: func(string, string) error { return nil },
			PrepareHost:     func(string, string, bool, bool, io.Writer) error { return nil },
		},
	}
	return s, s.handler()
}

func TestGuardRejectsWithoutToken(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("GET", "http://127.0.0.1:47615/api/workspaces", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no token should be forbidden, got %d", rec.Code)
	}
}

func TestGuardRejectsWrongToken(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("GET", "http://127.0.0.1:47615/?t=wrong", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a wrong token should be forbidden, got %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong token must not hand out a session cookie")
	}
}

// The bootstrap: the token arrives once in the URL, becomes a Strict-SameSite
// cookie, and is stripped from the address bar by a redirect.
func TestGuardTokenInURLBecomesCookie(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("GET", "http://127.0.0.1:47615/?t=secret-token", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a valid token should redirect (strip it from the URL), got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("should redirect to a token-free URL, got %q", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected one session cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != cookieName || c.Value != "secret-token" {
		t.Errorf("unexpected cookie %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("the session cookie must be HttpOnly (script must not read it)")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie must be SameSite=Strict — that's what blocks CSRF")
	}
}

func TestGuardAcceptsCookie(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("GET", "http://127.0.0.1:47615/api/workspaces", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a valid cookie should be let through, got %d", rec.Code)
	}
}

// DNS rebinding: an attacker's name resolving to 127.0.0.1 still arrives with
// their Host header, which must be refused.
func TestGuardRejectsForeignHostHeader(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("GET", "http://evil.example/api/workspaces", nil)
	req.Host = "evil.example"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-loopback Host must be refused, got %d", rec.Code)
	}
}

// CSRF: even holding the cookie, a write driven from another origin is refused.
func TestGuardRejectsCrossOriginWrite(t *testing.T) {
	_, h := testServer(t)
	req := httptest.NewRequest("POST", "http://127.0.0.1:47615/api/ws/x/stop", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross-origin write must be refused, got %d", rec.Code)
	}
}

func TestGuardAllowsSameOriginWrite(t *testing.T) {
	_, h := testServer(t)
	// HostFor returns nil in the test deps, so the handler answers 404 — which is
	// past the guard, and that's what we're asserting.
	req := httptest.NewRequest("POST", "http://127.0.0.1:47615/api/ws/x/stop", nil)
	req.Header.Set("Origin", "http://127.0.0.1:47615")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Error("a same-origin write must pass the guard")
	}
}

// Two browsers following one job must each receive the whole output — a second
// follower joining late still gets everything from the start.
func TestJobSupportsConcurrentFollowers(t *testing.T) {
	j := newJob()
	_, _ = io.WriteString(j, "first\n")

	follow := func() string {
		var got strings.Builder
		i := 0
		for {
			chunks, next, done, wait, _ := j.since(i)
			i = next
			for _, c := range chunks {
				got.WriteString(c)
			}
			if done {
				return got.String()
			}
			<-wait
		}
	}

	var wg sync.WaitGroup
	results := make([]string, 2)
	for k := range results {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k] = follow()
		}(k)
	}

	_, _ = io.WriteString(j, "second\n")
	j.finish(nil)
	wg.Wait()

	for k, got := range results {
		if got != "first\nsecond\n" {
			t.Errorf("follower %d saw %q, want the full output from the start", k, got)
		}
	}
}
