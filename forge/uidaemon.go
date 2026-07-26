package forge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/proc"
)

// The browser UI daemon's lifecycle. The daemon itself is the ui package — this
// is only what starts it, finds it and stops it, which every front end needs and
// none of them should each have its own version of.

// UIDaemon is the browser UI's state on this machine: whether it is up, and what
// it takes to reach it.
type UIDaemon struct {
	Running bool
	PID     int
	// Port is the configured port. For a running daemon that is the port it bound;
	// for a stopped one it is the port the next start will try.
	Port int
	// Token is the session token the running daemon minted for itself, empty when
	// nothing is running. Read back from disk rather than assumed: the daemon that
	// won the port is the one that wrote it, so this is the token actually being
	// served even if two starts raced.
	Token string
}

// UIStatus reports whether the browser UI is running, and how to reach it.
func UIStatus() (UIDaemon, error) {
	dir, err := config.Dir()
	if err != nil {
		return UIDaemon{}, err
	}
	cfg, err := config.Load()
	if err != nil {
		return UIDaemon{}, err
	}
	d := UIDaemon{Port: cfg.UIPortOr()}
	pid, ok := daemonPID(UIPIDPath(dir))
	if !ok {
		return d, nil
	}
	d.Running, d.PID, d.Token = true, pid, readUIToken(dir)
	return d, nil
}

// StartUI starts the browser UI daemon and returns it once it is actually
// serving, reporting whether it was already up.
//
// It waits for the daemon's pidfile, which is written only after a successful
// bind. That is what turns "the port is already in use" into an error here
// rather than into a browser opening on a dead address.
func StartUI() (d UIDaemon, already bool, err error) {
	dir, err := config.Dir()
	if err != nil {
		return UIDaemon{}, false, err
	}
	cur, err := UIStatus()
	if err != nil {
		return UIDaemon{}, false, err
	}
	if cur.Running {
		return cur, true, nil
	}

	if err := startDetached(dir, "ui.log", RunUIArg); err != nil {
		return UIDaemon{}, false, err
	}
	if !awaitPID(UIPIDPath(dir), true, 3*time.Second) {
		return UIDaemon{}, false, fmt.Errorf("the UI daemon didn't come up (port %d may be in use)\n  see %s",
			cur.Port, filepath.Join(dir, "ui.log"))
	}
	d, err = UIStatus()
	return d, false, err
}

// StopUI signals the browser UI daemon and waits for it to go. Reports false if
// none was running.
//
// The wait is not politeness: we tell people to run `forge ui stop && forge ui`,
// and a replacement that starts while the old one still holds the port fails to
// bind.
func StopUI() (bool, error) {
	dir, err := config.Dir()
	if err != nil {
		return false, err
	}
	pid, ok := daemonPID(UIPIDPath(dir))
	if !ok {
		return false, nil
	}
	if err := proc.Terminate(pid); err != nil {
		return false, err
	}
	awaitPID(UIPIDPath(dir), false, 3*time.Second)
	return true, nil
}

// readUIToken reads the token the running daemon minted for itself. Absent means
// no token, which yields a URL that will ask for one.
func readUIToken(dir string) string {
	data, err := os.ReadFile(UITokenPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
