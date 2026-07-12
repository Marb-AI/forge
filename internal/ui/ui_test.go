package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
