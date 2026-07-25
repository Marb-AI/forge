package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestUsageEndpointReturnsMap(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceUsage = func() (map[string]Usage, error) {
		return map[string]Usage{"api": {
			Account:     Account{UUID: "u-1", Email: "dev@example.com", Org: "Example Ltd"},
			Auth:        "subscription",
			TS:          1700000000,
			Model:       "Opus 5",
			ContextUsed: 128471,
			ContextSize: 200000,
			CostUSD:     1.25,
			FiveHour:    &RateWindow{UsedPercent: 23.5, ResetsAt: 1738425600},
			SevenDay:    &RateWindow{UsedPercent: 41.2, ResetsAt: 1738857600},
		}}, nil
	}
	rec := authorized(h, "/api/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]Usage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	u := got["api"]
	if u.Account.UUID != "u-1" || u.Account.Email != "dev@example.com" {
		t.Errorf("login not passed through: %+v", u.Account)
	}
	if u.ContextUsed != 128471 || u.ContextSize != 200000 || u.CostUSD != 1.25 {
		t.Errorf("per-workspace figures not passed through: %+v", u)
	}
	if u.FiveHour == nil || u.FiveHour.UsedPercent != 23.5 || u.SevenDay == nil {
		t.Errorf("rate windows not passed through: %+v / %+v", u.FiveHour, u.SevenDay)
	}
	// Polled endpoint: no caching, or a group would keep showing a limit it has
	// already moved past.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// An absent rate window must reach the browser as absent. A company on API credits
// has no 5-hour window at all, and a zeroed one would render as a full allowance —
// the most misleading thing this panel could say.
func TestUsageEndpointKeepsAbsentWindowsAbsent(t *testing.T) {
	s, h := testServer(t)
	s.deps.WorkspaceUsage = func() (map[string]Usage, error) {
		return map[string]Usage{"api": {Auth: "api", TS: 1700000000, CostUSD: 12.5}}, nil
	}
	body := authorized(h, "/api/usage").Body.String()
	if strings.Contains(body, "five_hour") || strings.Contains(body, "seven_day") {
		t.Errorf("absent windows were serialised anyway: %s", body)
	}
	// The login is absent too, for the same reason — there isn't one.
	var got map[string]Usage
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, body)
	}
	if got["api"].Account.UUID != "" || got["api"].Auth != "api" {
		t.Errorf("credit workspace = %+v, want no login and the auth kind that explains why", got["api"])
	}
}

// A host we can't reach must leave the panel alone, not fail the poll.
func TestUsageEndpointNilDep(t *testing.T) {
	_, h := testServer(t)
	rec := authorized(h, "/api/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("nil WorkspaceUsage should 200 empty, got %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
		t.Errorf("body = %s, want an empty object", body)
	}
}
