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

// prepareCapture builds a server whose PrepareHost records what it was asked for.
func prepareCapture(t *testing.T) (http.Handler, func() (firewall, harden, prune bool, called bool)) {
	t.Helper()
	var mu sync.Mutex
	var fw, hd, pr, called bool

	s := &server{
		token:     "secret-token",
		terms:     newTermRegistry(),
		ckRunning: map[string]bool{},
		jobs:      map[string]*job{},
		deps: Deps{
			ListWorkspaces:  func() ([]WorkspaceInfo, error) { return nil, nil },
			HostFor:         func(string) *config.Host { return nil },
			Checkpoint:      func(string, io.Writer) error { return nil },
			ListHosts:       func() ([]string, error) { return nil, nil },
			CreateWorkspace: func(string, string) error { return nil },
			DeleteWorkspace: func(string) error { return nil },
			RemoveHost:      func(string) error { return nil },
			SetUIPort:       func(int) error { return nil },
			PrepareHost: func(_, _ string, firewall, harden, prune bool, _ io.Writer) error {
				mu.Lock()
				fw, hd, pr, called = firewall, harden, prune, true
				mu.Unlock()
				return nil
			},
		},
	}
	get := func() (bool, bool, bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return fw, hd, pr, called
	}
	return s.handler(), get
}

func postPrepare(t *testing.T, h http.Handler, body string) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:47615/api/hosts/prepare", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:47615")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "secret-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("prepare should have been accepted, got %d: %s", rec.Code, rec.Body)
	}
}

// The hardening flags guard a real machine. A request that simply forgets to send
// one must not quietly provision a server with no firewall — absent has to mean
// the safe default, exactly as it does on the command line.
func TestPrepareDefaultsToHardenedWhenFlagsAbsent(t *testing.T) {
	h, got := prepareCapture(t)
	postPrepare(t, h, `{"target":"root@1.2.3.4","alias":"box"}`)

	fw, hd, pr, called := waitCalled(t, got)
	if !called {
		t.Fatal("PrepareHost was never called")
	}
	if !fw {
		t.Error("a request with no firewall field must still install the firewall")
	}
	if !hd {
		t.Error("a request with no harden field must still harden SSH")
	}
	if !pr {
		t.Error("a request with no dockerPrune field must still schedule the clean-up")
	}
}

// …and an explicit false must still be honoured, or the checkboxes are decoration.
func TestPrepareHonoursExplicitFalse(t *testing.T) {
	h, got := prepareCapture(t)
	postPrepare(t, h, `{"target":"root@1.2.3.4","alias":"box","firewall":false,"harden":false,"dockerPrune":false}`)

	fw, hd, pr, called := waitCalled(t, got)
	if !called {
		t.Fatal("PrepareHost was never called")
	}
	if fw || hd || pr {
		t.Errorf("explicit false must be honoured, got firewall=%v harden=%v prune=%v", fw, hd, pr)
	}
}

// The job runs in a goroutine; give it a moment to land.
func waitCalled(t *testing.T, got func() (bool, bool, bool, bool)) (bool, bool, bool, bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if fw, hd, pr, called := got(); called {
			return fw, hd, pr, called
		}
		time.Sleep(5 * time.Millisecond)
	}
	return got()
}
