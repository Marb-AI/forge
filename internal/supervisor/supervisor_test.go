package supervisor

import "testing"

func TestIsAuthFailure(t *testing.T) {
	yes := []string{
		"Permission denied (publickey).",
		"ssh: Too many authentication failures",
		"PUBLICKEY denied",
	}
	for _, s := range yes {
		if !isAuthFailure(s) {
			t.Errorf("expected auth failure: %q", s)
		}
	}
	no := []string{"Connection refused", "no route to host", ""}
	for _, s := range no {
		if isAuthFailure(s) {
			t.Errorf("did not expect auth failure: %q", s)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("solo"); got != "solo" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Supervisor{dir: dir, state: map[key]*TunnelStatus{}}
	s.set(key{"myserver", "crm", 3000}, StateUp, "")
	s.set(key{"myserver", "crm", 5173}, StateRetrying, "connection refused")
	s.writeStatus()

	st, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Tunnels) != 2 {
		t.Fatalf("expected 2 tunnels, got %d", len(st.Tunnels))
	}
	found := map[int]string{}
	for _, tn := range st.Tunnels {
		found[tn.Port] = tn.State
	}
	if found[3000] != StateUp || found[5173] != StateRetrying {
		t.Errorf("unexpected states: %v", found)
	}
}
