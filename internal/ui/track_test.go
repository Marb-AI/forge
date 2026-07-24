package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/config"
)

func TestTrackEndpointReturnsMap(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceTrack = func() (map[string]Track, error) {
		return map[string]Track{"api": {SessionStart: 1700000000, ActiveSeconds: 300}}, nil
	}
	rec := authorized(h, "/api/track")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]Track
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if got["api"].SessionStart != 1700000000 || got["api"].ActiveSeconds != 300 {
		t.Errorf("track not passed through: %+v", got)
	}
	// Polled endpoint: no caching, or the clocks would read stale.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// A host we can't reach must leave the clocks alone, not 500 the poll.
func TestTrackEndpointNilDep(t *testing.T) {
	_, h := testServer(t)
	if rec := authorized(h, "/api/track"); rec.Code != http.StatusOK {
		t.Fatalf("nil WorkspaceTrack should 200 empty, got %d", rec.Code)
	}
}

// authorizedPost issues a cookie-carrying POST with a JSON body (no Origin header,
// which sameOrigin treats as same-origin — see the guard tests).
func authorizedPost(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://127.0.0.1:47615"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTrackIncForwardsSeconds(t *testing.T) {
	s, h := testServer(t)
	s.deps.HostFor = func(string) *config.Host { return &config.Host{} }
	var gotWs string
	var gotSecs int
	s.deps.TrackInc = func(ws string, secs int) error { gotWs, gotSecs = ws, secs; return nil }

	rec := authorizedPost(h, "/api/track/api/inc", `{"seconds":42}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if gotWs != "api" || gotSecs != 42 {
		t.Errorf("TrackInc got (%q, %d), want (api, 42)", gotWs, gotSecs)
	}
}

func TestTrackIncUnknownWorkspace404(t *testing.T) {
	s, h := testServer(t)
	s.deps.HostFor = func(string) *config.Host { return nil } // unknown workspace
	called := false
	s.deps.TrackInc = func(string, int) error { called = true; return nil }

	rec := authorizedPost(h, "/api/track/ghost/inc", `{"seconds":10}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if called {
		t.Error("TrackInc must not run for an unknown workspace")
	}
}

func TestTrackIncZeroIsNoop(t *testing.T) {
	s, h := testServer(t)
	s.deps.HostFor = func(string) *config.Host { return &config.Host{} }
	called := false
	s.deps.TrackInc = func(string, int) error { called = true; return nil }

	rec := authorizedPost(h, "/api/track/api/inc", `{"seconds":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if called {
		t.Error("a zero delta should not hit the agent")
	}
}
