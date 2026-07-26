package forge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A pidfile is the only thing that answers "is the daemon running?", and every
// start, stop and status goes through this reading of it. The load-bearing part
// is the stale case: a daemon that was killed leaves its pidfile behind, and a
// pidfile read as "running" on the strength of existing alone would make the UI
// unstartable until someone deleted a file they have never heard of.
func TestDaemonPIDIgnoresWhatIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, ok := daemonPID(filepath.Join(dir, "absent.pid")); ok {
		t.Error("a missing pidfile must read as not running")
	}
	if _, ok := daemonPID(write("stale.pid", strconv.Itoa(1<<30))); ok {
		t.Error("a pidfile naming a dead process must read as not running")
	}
	if _, ok := daemonPID(write("junk.pid", "not a number")); ok {
		t.Error("an unreadable pidfile must read as not running")
	}
	if _, ok := daemonPID(write("zero.pid", "0")); ok {
		t.Error("pid 0 must read as not running")
	}

	// The one case that is running: this very process, written the way a daemon
	// writes it (trailing newline and all).
	live := write("live.pid", strconv.Itoa(os.Getpid())+"\n")
	pid, ok := daemonPID(live)
	if !ok || pid != os.Getpid() {
		t.Errorf("daemonPID = %d, %v; want this process", pid, ok)
	}
}

// awaitPID is what makes `forge ui stop && forge ui` work, and what turns a port
// that is already taken into an error instead of a browser opening on a dead
// address. It has to be able to answer both questions, and to give up.
func TestAwaitPIDWaitsInBothDirections(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.pid")
	if err := os.WriteFile(live, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	if !awaitPID(live, true, 200*time.Millisecond) {
		t.Error("a live pidfile should be seen as up")
	}
	if awaitPID(live, false, 200*time.Millisecond) {
		t.Error("a pidfile that never goes away must time out, not report gone")
	}

	absent := filepath.Join(dir, "absent.pid")
	if !awaitPID(absent, false, 200*time.Millisecond) {
		t.Error("no pidfile should be seen as gone")
	}
	if awaitPID(absent, true, 200*time.Millisecond) {
		t.Error("a daemon that never comes up must time out, not report up")
	}

	// And it must notice a change rather than only the state it started in: this
	// is the whole reason it is a wait and not a check.
	appears := filepath.Join(dir, "appears.pid")
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.WriteFile(appears, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}()
	if !awaitPID(appears, true, 3*time.Second) {
		t.Error("a pidfile written while waiting should be seen")
	}
}
