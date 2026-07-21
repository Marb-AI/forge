package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Marb-AI/forge/internal/config"
)

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"/", "", true},
		{".", "", true},
		{"src", "src", true},
		{"/src/main.go", "src/main.go", true},
		{"src//main.go", "src/main.go", true},
		{"src/./main.go", "src/main.go", true},
		{"src/sub/../main.go", "src/main.go", true}, // stays inside: fine
		// Escapes: every one of these must be refused.
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"src/../../etc", "", false},
		{"a/../../..", "", false},
	}
	for _, c := range cases {
		got, ok := cleanRel(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("cleanRel(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestShQuote(t *testing.T) {
	// The point is that nothing can break out of the single quotes.
	cases := map[string]string{
		"main.go":       `'main.go'`,
		"my file.txt":   `'my file.txt'`,
		"a;rm -rf /":    `'a;rm -rf /'`,
		"it's":          `'it'\''s'`,
		"$(whoami)":     `'$(whoami)'`,
		"`id`":          "'`id`'",
		"a'; touch x;'": `'a'\''; touch x;'\'''`,
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"a", "app", "marbai-01", "my_ws", "A1", strings.Repeat("x", 32)}
	for _, s := range valid {
		if !validName(s) {
			t.Errorf("validName(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",                      // empty
		strings.Repeat("x", 33), // too long
		"has space", "dot.name", // would not be a clean linux user
		"semi;colon", "slash/es", "quote'", "../up", "emoji🙂",
	}
	for _, s := range invalid {
		if validName(s) {
			t.Errorf("validName(%q) = true, want false", s)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	// The DNS-rebinding defense: only loopback names may reach the UI.
	ok := []string{"localhost", "localhost:47615", "127.0.0.1", "127.0.0.1:47615", "[::1]:47615", "127.0.0.2"}
	for _, h := range ok {
		if !loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = false, want true", h)
		}
	}
	bad := []string{"evil.com", "evil.com:47615", "10.0.0.5", "192.168.1.9:47615", ""}
	for _, h := range bad {
		if loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = true, want false", h)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	req := func(origin string) *http.Request {
		r := httptest.NewRequest("POST", "http://127.0.0.1:47615/api/ws/x/stop", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	if !sameOrigin(req("http://127.0.0.1:47615")) {
		t.Error("our own origin must be allowed")
	}
	if !sameOrigin(req("")) {
		t.Error("no Origin means no browser-driven cross-site request; it must be allowed")
	}

	// The hole this test exists for. A page on ANOTHER localhost port is the same
	// *site* as far as SameSite is concerned, so it gets our cookie; and a POST
	// with Content-Type: text/plain is CORS-"simple", so it is sent with no
	// preflight to stop it. Only an exact origin match keeps it out.
	crossPort := []string{
		"http://127.0.0.1:9999",
		"http://127.0.0.1:3000",
		"http://localhost:47615", // same name, but not the host we are serving on
		"http://localhost:5173",
	}
	for _, o := range crossPort {
		if sameOrigin(req(o)) {
			t.Errorf("sameOrigin(%q) = true — another local origin could drive the UI", o)
		}
	}

	for _, o := range []string{"http://evil.com", "https://127.0.0.1:47615", "null", "not a url"} {
		if sameOrigin(req(o)) {
			t.Errorf("sameOrigin(%q) = true, want false", o)
		}
	}
}

func TestParseDim(t *testing.T) {
	cases := []struct {
		in   string
		want uint16
	}{
		{"80", 80},
		{"200", 200},
		{"", 24},      // absent -> default
		{"abc", 24},   // garbage -> default
		{"0", 24},     // a zero pty would make tmux render into nothing
		{"-5", 24},    // negative
		{"99999", 24}, // absurd -> default, never an enormous pty
	}
	for _, c := range cases {
		if got := parseDim(c.in, 24); got != c.want {
			t.Errorf("parseDim(%q, 24) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestValidKindAndTermKey(t *testing.T) {
	if !validKind(termClaude) || !validKind(termSSH) {
		t.Error("claude and ssh must be valid kinds")
	}
	if validKind("bogus") || validKind("") {
		t.Error("unknown kinds must be refused")
	}
	// The Claude terminal and the ssh shell of one workspace must not collide.
	if termKey("ws", termClaude) == termKey("ws", termSSH) {
		t.Error("terminal kinds must be namespaced apart in the registry")
	}
}

func TestTermRegistryReplaceAndRemove(t *testing.T) {
	r := newTermRegistry()
	a, b := &term{}, &term{}

	r.replace("k", a)
	if r.get("k") != a {
		t.Fatal("replace should install the terminal")
	}
	r.replace("k", b)
	if r.get("k") != b {
		t.Fatal("replace should supersede the previous terminal")
	}
	// A stale handler must not evict the terminal that replaced it.
	if r.remove("k", a) {
		t.Error("remove of a superseded terminal should be a no-op")
	}
	if r.get("k") != b {
		t.Error("the live terminal was evicted by a stale one")
	}
	if !r.remove("k", b) {
		t.Error("remove of the live terminal should report it removed")
	}
	if r.get("k") != nil {
		t.Error("terminal should be gone after remove")
	}
}

func TestJobStreamsAndFinishes(t *testing.T) {
	j := newJob()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.WriteString(j, "step one\n")
		_, _ = io.WriteString(j, "step two\n")
		j.finish(nil)
	}()

	// Follow it the way the SSE handler does, until done.
	var got strings.Builder
	i := 0
	deadline := time.After(2 * time.Second)
	for {
		chunks, next, done, wait, err := j.since(i)
		i = next
		for _, c := range chunks {
			got.WriteString(c)
		}
		if done {
			if err != nil {
				t.Fatalf("unexpected job error: %v", err)
			}
			break
		}
		select {
		case <-wait:
		case <-deadline:
			t.Fatal("job never finished")
		}
	}
	wg.Wait()

	if got.String() != "step one\nstep two\n" {
		t.Errorf("followed output = %q, want both steps in order", got.String())
	}
}

func TestJobReportsFailure(t *testing.T) {
	// A job that fails must surface the error — a silently-swallowed failure is
	// what makes a spinner hang forever.
	j := newJob()
	j.finish(io.ErrUnexpectedEOF)

	_, _, done, _, err := j.since(0)
	if !done {
		t.Fatal("finished job should report done")
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("job error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

func TestDepsValidateCatchesUnwiredOps(t *testing.T) {
	full := Deps{
		ListWorkspaces:  func() ([]WorkspaceInfo, error) { return nil, nil },
		HostFor:         func(string) *config.Host { return nil },
		Checkpoint:      func(string, io.Writer) error { return nil },
		ListHosts:       func() ([]string, error) { return nil, nil },
		CreateWorkspace: func(string, string) error { return nil },
		PrepareHost:     func(string, string, bool, bool, bool, io.Writer) error { return nil },
		DeleteWorkspace: func(string) error { return nil },
		RemoveHost:      func(string) error { return nil },
		SetUIPort:       func(int) error { return nil },
	}
	if err := full.validate(); err != nil {
		t.Fatalf("a fully wired Deps should validate, got %v", err)
	}

	// Every field is required — dropping any one must be caught at startup rather
	// than panicking inside a handler later.
	drops := map[string]func(*Deps){
		"ListWorkspaces":  func(d *Deps) { d.ListWorkspaces = nil },
		"HostFor":         func(d *Deps) { d.HostFor = nil },
		"Checkpoint":      func(d *Deps) { d.Checkpoint = nil },
		"ListHosts":       func(d *Deps) { d.ListHosts = nil },
		"CreateWorkspace": func(d *Deps) { d.CreateWorkspace = nil },
		"PrepareHost":     func(d *Deps) { d.PrepareHost = nil },
		"DeleteWorkspace": func(d *Deps) { d.DeleteWorkspace = nil },
		"RemoveHost":      func(d *Deps) { d.RemoveHost = nil },
		"SetUIPort":       func(d *Deps) { d.SetUIPort = nil },
	}
	for name, drop := range drops {
		d := full
		drop(&d)
		err := d.validate()
		if err == nil {
			t.Errorf("Deps without %s should not validate", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for missing %s should name it, got %q", name, err)
		}
	}
}

func TestCheckpointGuardIsSingleFlight(t *testing.T) {
	s := &server{ckRunning: map[string]bool{}}
	if !s.beginCheckpoint("ws") {
		t.Fatal("first checkpoint should be allowed")
	}
	if s.beginCheckpoint("ws") {
		t.Error("a second concurrent checkpoint on the same workspace must be refused")
	}
	if !s.beginCheckpoint("other") {
		t.Error("a different workspace should be unaffected")
	}
	s.endCheckpoint("ws")
	if !s.beginCheckpoint("ws") {
		t.Error("after finishing, a new checkpoint should be allowed again")
	}
}

// A reconnect storm is many UI tabs asking "is my session still alive?" at once,
// and answering costs an SSH round trip per host. If every asker got its own
// call, an outage would produce a burst of simultaneous handshakes — exactly what
// sshd's MaxStartups refuses, so the storm would keep itself from recovering.
// Callers that arrive while a call is in flight must share its answer.
//
// The synchronisation is exact, not timed. The leader's call blocks on `release`,
// and each of the other callers signals onJoin the instant it commits to waiting
// on that call — so the test releases the leader only once all askers-1 of them
// have joined. There is no sleep to lose a race to, and no spin that can hang: a
// caller that failed to join would leave `joined` short of its target and trip
// the bounded wait instead of silently starting a second call.
func TestConcurrentWorkspaceListsShareOneCall(t *testing.T) {
	const askers = 20

	var calls atomic.Int32
	release := make(chan struct{})
	joined := make(chan struct{}, askers)
	s := &server{
		onJoin: func() { joined <- struct{}{} },
		deps: Deps{ListWorkspaces: func() ([]WorkspaceInfo, error) {
			calls.Add(1)
			<-release // hold the leader's call open so the others pile up behind it
			return []WorkspaceInfo{{Name: "w17-01", Host: "h", Status: "running"}}, nil
		}},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	got := make([][]WorkspaceInfo, askers)
	for i := range askers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so they genuinely contend
			list, err := s.listWorkspacesShared(0)
			if err != nil {
				t.Errorf("asker %d: %v", i, err)
			}
			got[i] = list
		}()
	}
	close(start)

	// Wait for exactly the askers-1 joiners (one asker is the leader running the
	// call). Bounded, so a regression that lets callers start their own call hangs
	// here for a second and fails, rather than spinning forever.
	waitJoins(t, joined, askers-1, 2*time.Second)
	close(release)
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("%d tabs asking at once produced %d SSH calls, want 1", askers, n)
	}
	// Sharing is only correct if everyone actually gets the answer.
	for i, list := range got {
		if len(list) != 1 || list[0].Name != "w17-01" {
			t.Errorf("asker %d got %v, want the shared result", i, list)
		}
	}

	// And the sharing must not turn into a cache: a later ask re-measures.
	release = make(chan struct{})
	close(release)
	if _, err := s.listWorkspacesShared(0); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a call after the first settled made %d total calls, want 2 (no caching)", n)
	}
}

// waitJoins blocks until n signals arrive on ch, or fails the test at the
// deadline — the bounded counterpart to spinning until some condition holds.
func waitJoins(t *testing.T, ch <-chan struct{}, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for range n {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out waiting for %d callers to join the in-flight call", n)
		}
	}
}

// Connectivity is a property of the server, not of each workspace on it: twenty
// tabs watching the same host through an outage are asking one question. A probe
// may therefore reuse a recent answer — but only a probe, and only a successful
// one, and never past the window.
func TestProbesReuseARecentListButActionsMeasure(t *testing.T) {
	var calls atomic.Int32
	fail := false
	clock := time.Unix(0, 0)
	s := &server{
		now: func() time.Time { return clock },
		deps: Deps{ListWorkspaces: func() ([]WorkspaceInfo, error) {
			calls.Add(1)
			if fail {
				return nil, io.ErrUnexpectedEOF
			}
			return []WorkspaceInfo{{Name: "w17-01", Host: "h", Status: "running"}}, nil
		}},
	}
	probe := func() { _, _ = s.listWorkspacesShared(10 * time.Second) }

	probe()
	if calls.Load() != 1 {
		t.Fatalf("first probe made %d calls, want 1", calls.Load())
	}
	// Twenty tabs, spread over the window rather than simultaneous — so this is
	// the reuse window doing the work, not the single-flight.
	for range 20 {
		clock = clock.Add(400 * time.Millisecond)
		probe()
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("20 probes within the window made %d SSH calls, want 1", n)
	}

	// Past the window, a probe measures again.
	clock = clock.Add(10 * time.Second)
	probe()
	if n := calls.Load(); n != 2 {
		t.Errorf("a probe past the window made %d total calls, want 2", n)
	}

	// An action never reuses: it is about to be acted on.
	before := calls.Load()
	if _, err := s.listWorkspacesShared(0); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != before+1 {
		t.Error("maxAge=0 must always measure — a status you act on cannot be remembered")
	}

	// A failure must not be reused: the server would keep being reported down for
	// the whole window after it had already come back.
	fail = true
	clock = clock.Add(time.Minute)
	probe()
	fail = false
	before = calls.Load()
	probe()
	if n := calls.Load(); n != before+1 {
		t.Error("a failed probe was reused; a recovered server would look down until it expired")
	}
}

// The window is a promise about staleness, so an oversized (or junk) request for
// one must not be honoured verbatim.
func TestMaxAgeIsClamped(t *testing.T) {
	cases := map[string]time.Duration{
		"":       0,
		"junk":   0,
		"0":      0,
		"-5":     0,
		"10":     10 * time.Second,
		"999999": maxAgeCap,
	}
	for in, want := range cases {
		if got := parseMaxAge(in); got != want {
			t.Errorf("parseMaxAge(%q) = %v, want %v", in, got, want)
		}
	}
}
