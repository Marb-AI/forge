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

type prepareArgs struct {
	firewall, harden, prune, pruneImages, called bool
}

// prepareCapture builds a server whose PrepareHost records what it was asked for.
func prepareCapture(t *testing.T) (http.Handler, func() prepareArgs) {
	t.Helper()
	var mu sync.Mutex
	var got prepareArgs

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
			PrepareHost: func(_, _ string, firewall, harden, prune, pruneImages bool, _ io.Writer) error {
				mu.Lock()
				got = prepareArgs{firewall, harden, prune, pruneImages, true}
				mu.Unlock()
				return nil
			},
		},
	}
	read := func() prepareArgs {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
	return s.handler(), read
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
// the safe default, exactly as it does on the command line. The image sweep is the
// exception: it is opt-in, so absent means off.
func TestPrepareDefaultsToHardenedWhenFlagsAbsent(t *testing.T) {
	h, got := prepareCapture(t)
	postPrepare(t, h, `{"target":"root@1.2.3.4","alias":"box"}`)

	a := waitCalled(t, got)
	if !a.called {
		t.Fatal("PrepareHost was never called")
	}
	if !a.firewall {
		t.Error("a request with no firewall field must still install the firewall")
	}
	if !a.harden {
		t.Error("a request with no harden field must still harden SSH")
	}
	if !a.prune {
		t.Error("a request with no dockerPrune field must still schedule the clean-up")
	}
	if a.pruneImages {
		t.Error("a request with no pruneImages field must NOT run the aggressive image sweep — it is opt-in")
	}
}

// …and an explicit false must still be honoured, or the checkboxes are decoration.
func TestPrepareHonoursExplicitFalse(t *testing.T) {
	h, got := prepareCapture(t)
	postPrepare(t, h, `{"target":"root@1.2.3.4","alias":"box","firewall":false,"harden":false,"dockerPrune":false}`)

	a := waitCalled(t, got)
	if !a.called {
		t.Fatal("PrepareHost was never called")
	}
	if a.firewall || a.harden || a.prune {
		t.Errorf("explicit false must be honoured, got firewall=%v harden=%v prune=%v", a.firewall, a.harden, a.prune)
	}
}

// The opt-in image sweep is off unless explicitly asked for — and then it must reach
// the provisioner, or the checkbox does nothing.
func TestPrepareHonoursExplicitPruneImages(t *testing.T) {
	h, got := prepareCapture(t)
	postPrepare(t, h, `{"target":"root@1.2.3.4","alias":"box","pruneImages":true}`)

	a := waitCalled(t, got)
	if !a.called {
		t.Fatal("PrepareHost was never called")
	}
	if !a.pruneImages {
		t.Error("pruneImages:true must be passed through to the provisioner")
	}
}

// The job runs in a goroutine; give it a moment to land.
func waitCalled(t *testing.T, got func() prepareArgs) prepareArgs {
	t.Helper()
	for i := 0; i < 200; i++ {
		if a := got(); a.called {
			return a
		}
		time.Sleep(5 * time.Millisecond)
	}
	return got()
}
