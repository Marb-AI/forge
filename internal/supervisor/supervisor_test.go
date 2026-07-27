package supervisor

import "testing"

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
	s := &Supervisor{dir: dir, state: map[key]*TunnelStatus{}, workers: map[key]*worker{}}
	// A status row belongs to a supervised tunnel: set() ignores keys with no
	// worker, so that a stopped tunnel cannot write itself back into the file.
	a, b := key{"myserver", "crm", 3000}, key{"myserver", "crm", 5173}
	s.workers[a] = &worker{cancel: func() {}, done: make(chan struct{})}
	s.workers[b] = &worker{cancel: func() {}, done: make(chan struct{})}
	s.set(a, StateUp, "")
	s.set(b, StateRetrying, "connection refused")
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
