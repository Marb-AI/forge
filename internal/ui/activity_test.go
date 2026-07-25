package ui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authorized issues a request that carries the session cookie, so it lands on the
// handler rather than the guard.
func authorized(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://127.0.0.1:47615"+path, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestActivityEndpointReturnsMap(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceActivity = func() (map[string]Activity, error) {
		return map[string]Activity{"api": {State: "idle", TS: 1784386328}}, nil
	}
	rec := authorized(h, "/api/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if got["api"].State != "idle" || got["api"].TS != 1784386328 {
		t.Errorf("activity not passed through: %+v", got)
	}
	// Polled endpoint: must forbid caching so marks can't go stale.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// The topic rides on the activity poll rather than one of its own: the browser
// already asks this endpoint every few seconds, and a second poll would be a second
// SSH round trip per host for a line of text that changes a few times a day.
func TestActivityEndpointCarriesTheTopic(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceActivity = func() (map[string]Activity, error) {
		return map[string]Activity{"api": {
			State: "busy", TS: 1784386328,
			Topic: "refactoring auth to use JWT", TopicTS: 1784386000,
		}}, nil
	}
	var got map[string]Activity
	rec := authorized(h, "/api/activity")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if got["api"].Topic != "refactoring auth to use JWT" || got["api"].TopicTS != 1784386000 {
		t.Errorf("topic not passed through: %+v", got["api"])
	}
	// The timestamp is what lets the UI dim a topic older than the session instead
	// of presenting last Tuesday's work as current.
	if !strings.Contains(rec.Body.String(), "topic_ts") {
		t.Errorf("topic_ts missing from the payload: %s", rec.Body)
	}
}

// A host we can't reach must dim tabs, not 500 the whole poll.
func TestActivityEndpointToleratesError(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceActivity = func() (map[string]Activity, error) {
		return nil, errors.New("ssh: connection refused")
	}
	rec := authorized(h, "/api/activity")
	if rec.Code != http.StatusOK {
		t.Fatalf("an unreachable host should still 200 with an empty map, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != "{}\n" && got != "{}" {
		t.Errorf("want empty map on error, got %q", got)
	}
}

// The endpoint must not panic when no provider is wired.
func TestActivityEndpointNilDep(t *testing.T) {
	_, h := testServer(t)
	if rec := authorized(h, "/api/activity"); rec.Code != http.StatusOK {
		t.Fatalf("nil WorkspaceActivity should 200 empty, got %d", rec.Code)
	}
}
