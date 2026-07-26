package ui

import (
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/Marb-AI/forge/forge"
)

// The UI without a daemon around it.
//
// On a laptop Forge is a command that leaves a background process behind: `forge
// ui` re-execs this binary detached, that process binds the port, writes a
// pidfile, and lives until something signals it. A desktop shell — Wails, and the
// same core on a phone after it — has no use for any of that. It IS the process,
// its window opens and closes with the UI, and it can no more re-exec a copy of
// itself on iOS than it can hand a browser a URL and walk away.
//
// So the server gets a second way in, alongside Serve: point the core at this
// device's state, bind a port, serve, and hand back what it takes to reach it and
// how to stop it. Everything below the seam is identical — the same handlers, the
// same token guard, the same core operations. What is missing is only the process
// furniture, and deliberately:
//
//   - No pidfile. It means "the browser UI daemon holds this machine's UI port",
//     and a desktop shell does not: writing one would make `forge ui stop` kill
//     the app, and a later `forge ui` believe it was already running.
//   - No token file. Whoever reads ~/.forge/ui.token wants the daemon's session,
//     and gets it: an in-process UI hands its token straight to the webview it
//     opened, so overwriting that file would only break the URL `forge ui status`
//     prints for a daemon that is genuinely running.
//   - No signal handler. The shell owns the process and its signals; Stop is what
//     it calls, whenever its own lifecycle says to.
//
// The three of them together are why a desktop app and the headless daemon can be
// up at once, on one machine, without either noticing the other.

// Instance is a UI running in this process: the port it took, the token that
// reaches it, and the way to stop it.
type Instance struct {
	// Port is the port actually bound — the one asked for, or the one the OS
	// picked when 0 was.
	Port int
	// Token is this run's session token. It is not written anywhere: hand it to
	// the webview (see URL) and it goes no further.
	Token string

	s   *server
	srv *http.Server

	done chan struct{} // closed when the serve loop has returned
	err  error         // why, unless it was a clean shutdown

	stopOnce sync.Once
	stopErr  error
}

// Start serves the UI in this process and returns once it is bound and
// answering. This is what a desktop shell calls, in place of spawning `forge ui`.
//
// dir is where this device's state lives; the core is pointed at it, exactly as
// `forge` points itself at ~/.forge. Empty means the caller has already said —
// with forge.Use, for a phone that keeps its config and key somewhere only it can
// name — and Start leaves that alone.
//
// port 0 asks the OS for a free one, which is the sane default for a shell that
// only needs its own webview to find it: it cannot collide with a running daemon,
// with a second window, or with whatever else is on the machine's loopback.
//
// The caller is expected to Stop it. Nothing else will: there is no signal
// handler here, and no pidfile for anyone to find it by.
func Start(dir string, port int) (*Instance, error) {
	if dir != "" {
		forge.Open(dir)
	}
	return start(port, CoreDeps())
}

// start is Start with the wiring handed in, so the tests can drive a real server
// over fakes instead of a real machine.
func start(port int, deps Deps) (*Instance, error) {
	ln, s, err := bind(port, deps)
	if err != nil {
		return nil, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("ui: listening on %v, which is not TCP", ln.Addr())
	}

	in := &Instance{
		Port: addr.Port, Token: s.token,
		s: s, srv: httpServer(s), done: make(chan struct{}),
	}
	go func() {
		defer close(in.done)
		if err := in.srv.Serve(ln); err != http.ErrServerClosed {
			in.err = err
		}
	}()
	return in, nil
}

// URL is the address to open this instance at, token and all.
func (in *Instance) URL() string { return URL(in.Port, in.Token) }

// Stop ends the UI: the terminals it opened are closed, the port is given back,
// and it returns once the serve loop has. Calling it twice is not an error — a
// shell that stops on a window close and again on quit is the normal case.
func (in *Instance) Stop() error {
	in.stopOnce.Do(func() {
		in.stopErr = shutdown(in.srv, in.s)
		<-in.done
		if in.stopErr == nil {
			in.stopErr = in.err
		}
	})
	return in.stopErr
}
