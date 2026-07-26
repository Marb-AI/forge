package ui

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What is left here is the local shell as a thing the browser has to keep
// straight: one shell for the whole machine, its own workspace-less endpoints,
// and a rail button that survives a workspace that does not. Opening it — a login
// shell, in your home — is the core's, and is tested there.

// The local shell belongs to no workspace, so it must not be able to collide with
// one in the terminal registry — a workspace whose ssh shell replaced the local
// one (or the reverse) would close a shell you were typing in.
func TestLocalTerminalCannotCollideWithAWorkspaceTerminal(t *testing.T) {
	for _, ws := range []string{"local", "", "crm"} {
		for _, kind := range []string{termClaude, termSSH, termHost} {
			if termKey(ws, kind) == localTermKey {
				t.Errorf("workspace %q's %s terminal shares the local shell's registry key", ws, kind)
			}
		}
	}
}

// The local shell's endpoints take no workspace — there is one local machine, so
// asking which workspace you mean is meaningless. This is what proves they are
// wired up at all: without the routes these would be the mux's own 404, not the
// handler's answer that no such terminal is open.
func TestLocalTerminalHasWorkspaceLessRoutes(t *testing.T) {
	_, h := testServer(t)

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/term/local/input", base64.StdEncoding.EncodeToString([]byte("x"))},
		{"POST", "/api/term/local/resize", `{"cols":80,"rows":24}`},
	} {
		req := httptest.NewRequest(c.method, "http://127.0.0.1:47615"+c.path, strings.NewReader(c.body))
		req.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (no local shell is open)", c.method, c.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "no terminal") {
			t.Errorf("%s %s answered %q — the route is not wired to the local terminal handler",
				c.method, c.path, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// One local machine, one local shell: every tab must resolve to the same session
// and the same ws-less endpoints. Keyed per workspace instead, each tab would
// silently start a shell of its own — five tabs, five shells, and the command you
// left running is in whichever one you opened it from.
func TestBrowserKeepsExactlyOneLocalShell(t *testing.T) {
	js := embeddedAsset(t, "app.js")

	body := jsFunc(t, js, "shellKey")
	if !strings.Contains(body, `"local"`) {
		t.Error("shellKey does not special-case the local shell; it would be keyed per workspace")
	}
	if !strings.Contains(jsFunc(t, js, "termPath"), "/api/term/local/") {
		t.Error("termPath does not send the local shell to its workspace-less endpoints")
	}
	// Deleting a workspace, or losing one from the list, must not take the local
	// shell with it — it is not that workspace's, and killing it would drop
	// whatever you were running on your own machine.
	if !strings.Contains(jsFunc(t, js, "disposeWsShells"), "WS_SHELL_KINDS") {
		t.Error("disposeWsShells sweeps every kind; a deleted workspace would close the local shell")
	}
	if !strings.Contains(jsFunc(t, js, "pruneShells"), "isLocalKind(sess.kind)") {
		t.Error("pruneShells would dispose the local shell: it matches no workspace by name")
	}
}

// The rail button is the whole feature from a user's side, and the local shell is
// the one that keeps working when a server does not — so it must not be greyed out
// with the rest when the active workspace is unreachable.
func TestLocalShellButtonStaysUsableWhenTheWorkspaceIsNot(t *testing.T) {
	if html := embeddedAsset(t, "index.html"); !strings.Contains(html, `data-action="local"`) {
		t.Fatal("index.html has no local-shell rail button")
	}
	body := jsFunc(t, embeddedAsset(t, "app.js"), "renderStage")
	if !strings.Contains(body, `"local"`) {
		t.Error("renderStage disables the local-shell button along with the workspace's own actions")
	}
}
