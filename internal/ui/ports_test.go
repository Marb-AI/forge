package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func portsServer(t *testing.T, d Deps) *server {
	t.Helper()
	return &server{deps: d}
}

func TestPortsHandlerReportsRows(t *testing.T) {
	s := portsServer(t, Deps{
		Ports: func(ws string) (WorkspacePortsInfo, error) {
			if ws != "crm" {
				t.Errorf("workspace = %q", ws)
			}
			return WorkspacePortsInfo{
				Block: "16000-16099",
				Rows: []PortRow{
					{Name: "web", Port: 16000, Target: 3000, Running: true, Kind: "container", Tunnel: "up", InBlock: true},
				},
			}, nil
		},
	})
	r := httptest.NewRequest("GET", "/api/ports/crm", nil)
	r.SetPathValue("ws", "crm")
	w := httptest.NewRecorder()
	s.handlePorts(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got WorkspacePortsInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Block != "16000-16099" || len(got.Rows) != 1 || got.Rows[0].Port != 16000 {
		t.Errorf("got %+v", got)
	}
	// Polled data must never be cached: the panel's whole job is saying what is
	// true now.
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

// A workspace with nothing published must serialise as [], not null: the browser
// iterates the rows, and null is a different shape.
func TestPortsHandlerEmptyRowsAreAnArray(t *testing.T) {
	s := portsServer(t, Deps{
		Ports: func(string) (WorkspacePortsInfo, error) { return WorkspacePortsInfo{}, nil },
	})
	r := httptest.NewRequest("GET", "/api/ports/crm", nil)
	r.SetPathValue("ws", "crm")
	w := httptest.NewRecorder()
	s.handlePorts(w, r)
	if !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestContainerActionValidates(t *testing.T) {
	called := 0
	s := portsServer(t, Deps{
		ContainerAction: func(ws, service, action string) error {
			called++
			if ws != "crm" || service != "web" || action != "stop" {
				t.Errorf("action(%q, %q, %q)", ws, service, action)
			}
			return nil
		},
	})

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/ports/crm/container", strings.NewReader(body))
		r.SetPathValue("ws", "crm")
		w := httptest.NewRecorder()
		s.handleContainerAction(w, r)
		return w
	}

	if w := post(`{"service":"web","action":"stop"}`); w.Code != http.StatusOK {
		t.Errorf("valid stop = %d: %s", w.Code, w.Body)
	}
	if called != 1 {
		t.Errorf("dep called %d times", called)
	}

	// `up` is the one that must never get through: creating containers needs to
	// know the project, which Forge does not.
	for _, body := range []string{
		`{"service":"web","action":"up"}`,
		`{"service":"web","action":"restart"}`,
		`{"service":"web","action":""}`,
		`{"service":"","action":"stop"}`,
		`{"service":"   ","action":"stop"}`,
		`not json`,
	} {
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Errorf("post(%s) = %d, want 400", body, w.Code)
		}
	}
	if called != 1 {
		t.Errorf("a rejected request reached the dep (called %d times)", called)
	}
}

// Every tunnel state the daemon can put in a row has to mean something in the
// browser. A state the JS does not name falls through to "connecting", which for
// a terminal auth failure is a spinner for something that will never connect.
func TestBrowserUnderstandsEveryTunnelStateGoCanSend(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	// The supervisor's vocabulary, plus the ui package's own "none". Spelled out
	// rather than imported because the ui package must not import the supervisor —
	// which is exactly why they can drift, and why this test exists.
	for _, state := range []string{"up", "blocked", "error", "none"} {
		if !regexp.MustCompile(`case "` + state + `":`).MatchString(js) {
			t.Errorf("app.js does not handle tunnel state %q by name", state)
		}
	}
	// And each state the panel derives has to paint something.
	for _, st := range []string{"ok", "stopped", "blocked", "error", "notunnel", "untunnelled", "connecting"} {
		if !regexp.MustCompile(`\b` + st + `:`).MatchString(js) {
			t.Errorf("PORT_DOT has no entry for %q", st)
		}
	}
}

// The non-HTTP heuristic has to run on the port INSIDE the container. The host
// port comes from the workspace's block and says nothing about what is behind it:
// Postgres published at 16003 is still Postgres, and linking to it is a dead click.
func TestNonHttpHeuristicUsesTheContainerPort(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	if !strings.Contains(js, "NON_HTTP_PORTS.has(p.target)") {
		t.Error("the non-HTTP check does not run on p.target")
	}
	if strings.Contains(js, "NON_HTTP_PORTS.has(p.port)") {
		t.Error("the non-HTTP check runs on the host port, which cannot say what the service is")
	}
	for _, p := range []string{"5432", "3306", "6379", "27017"} {
		if !strings.Contains(js, p) {
			t.Errorf("NON_HTTP_PORTS is missing %s", p)
		}
	}
}

// A link is only truthful while the tunnel is up. Anything else has to be a copy
// button, or the panel offers clicks that fail.
func TestLinkOnlyWhenTheTunnelIsUp(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	if !strings.Contains(js, `st === "ok" && !NON_HTTP_PORTS.has(p.target)`) {
		t.Error("portTarget links without checking that the tunnel is up")
	}
}

// The panel must not poll while nobody can see it — collapsed, or the tab in the
// background — the same discipline as the servers panel it sits above.
func TestPortsPanelDoesNotPollWhenHidden(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	if !strings.Contains(js, "!document.hidden && !portsCollapsed()") {
		t.Error("portsWanted does not park the poll for a hidden or collapsed panel")
	}
	m := regexp.MustCompile(`PORTS_POLL_MS\s*=\s*(\d+)`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js has no PORTS_POLL_MS")
	}
}

// A plain process has no container to start, so it must not be offered a button
// that could only fail.
func TestOnlyContainersGetAStartStopButton(t *testing.T) {
	js := embeddedAsset(t, "app.js")
	if !strings.Contains(js, `p.kind === "container"`) {
		t.Error("the start/stop button is not gated on the row being a container")
	}
}

func TestPortsPanelIsInTheDocument(t *testing.T) {
	html := embeddedAsset(t, "index.html")
	for _, id := range []string{`id="ports"`, `id="ports-head"`, `id="portlist"`, `id="ports-block"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html is missing %s", id)
		}
	}
	css := embeddedAsset(t, "app.css")
	if !strings.Contains(css, "#ports.collapsed #portlist { display: none; }") {
		t.Error("app.css has no rule hiding the list when the panel is collapsed")
	}
}
