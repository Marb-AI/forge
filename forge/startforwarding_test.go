package forge

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/supervisor"
)

// scratchState points the core at a directory of this test's own and puts the
// machine's back afterwards, so nothing here reads or writes the state of
// whatever it is running on.
func scratchState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	swapState(t, dir)
	if _, err := StateDir(); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	return dir
}

// A supervisor that is already running is left running.
//
// The reason is ports, not politeness: two supervisors on one machine want the
// same local numbers, and everything the second one puts up fails as
// ErrPortBusy. The one already there is forwarding the same ports this would, so
// there is nothing to take them for — and it belongs to whoever started it,
// which is not this window.
func TestAnApplicationLeavesARunningSupervisorAlone(t *testing.T) {
	dir := scratchState(t)

	// A pidfile naming a live process is exactly what "the supervisor is running"
	// means everywhere else in this package, so it is what the test writes. This
	// process will do: it is unquestionably alive.
	pid := os.Getpid()
	if err := os.WriteFile(supervisor.PIDPath(dir), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	tun, err := StartForwarding()
	if err != nil {
		t.Fatalf("StartForwarding with a supervisor up: %v", err)
	}
	if tun.in != nil {
		t.Error("started a second supervisor while one was running — its tunnels " +
			"would lose every local port to the one already holding them")
	}
	// And stopping a handle that holds nothing takes nothing down, which is what
	// lets a shell keep one without asking which kind it got.
	tun.Stop()
	if _, running := daemonPID(supervisor.PIDPath(dir)); !running {
		t.Error("closing the window stopped a supervisor it did not start")
	}
}

// With none running, the process holds the tunnels itself — and writes no
// pidfile doing it, because `forge forwarding stop` terminates whatever the
// pidfile names, and here that is the application.
func TestAnApplicationHoldsTheTunnelsItself(t *testing.T) {
	dir := scratchState(t)

	tun, err := StartForwarding()
	if err != nil {
		t.Fatalf("StartForwarding with nothing running: %v", err)
	}
	defer tun.Stop()

	if tun.in == nil {
		t.Fatal("nothing is carrying the tunnels: the ports panel would list " +
			"addresses that do not answer")
	}
	if _, err := os.Stat(supervisor.PIDPath(dir)); !os.IsNotExist(err) {
		t.Errorf("an in-process supervisor wrote %s — `forge forwarding stop` "+
			"would then terminate the application, window and all",
			supervisor.PIDPath(dir))
	}
	// It is not a daemon, so nothing else can find it: ForwardingStatus reads the
	// pidfile, and says what it sees.
	if st, err := ForwardingStatus(); err == nil && st.Running {
		t.Error("ForwardingStatus found a daemon that does not exist")
	}
}

// A supervisor that could not be started leaves no tunnels behind on screen.
//
// The ports panel reads the status file without asking whether anybody is
// holding those tunnels — see tunnelStates — so the file is only ever true
// because whatever ends a supervisor clears it. Failing to start one is the
// third way of ending up with none, and the one that matters most here: the
// caller carries on with TunnelErr set and puts a window up, so a stale file
// would show live tunnels for ports that answer nothing.
func TestFailingToStartTheTunnelsClearsWhatTheLastOneLeft(t *testing.T) {
	dir := scratchState(t)

	// What a previous supervisor would have left: a status file describing
	// tunnels, and no pidfile, because it is gone.
	stale := filepath.Join(dir, "status.json")
	if err := os.WriteFile(stale, []byte(`{"pid":1,"tunnels":[{"workspace":"w","port":16000}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A store with nowhere to keep state is the failure this path can actually
	// have; the directory is still known, so there is a file to clear.
	keys, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	prev, err := Store()
	if err != nil {
		t.Fatal(err)
	}
	if err := Use(halfStore{Store: prev}, keys); err != nil {
		t.Fatal(err)
	}

	if _, err := StartForwarding(); err == nil {
		t.Fatal("a store that cannot be loaded started tunnels anyway")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the panel would show tunnels from a supervisor that is not running")
	}
}

// halfStore knows its directory and cannot load — the shape that gets past
// StateDir and fails inside the supervisor.
type halfStore struct{ config.Store }

func (halfStore) Load() (*config.Config, error) { return nil, errCannotLoad }

var errCannotLoad = errors.New("this config cannot be read")

// Stop is called on window close and again on quit, so it has to survive being
// said twice — and a handle that was never started has to survive it too.
func TestStoppingTheTunnelsTwiceIsFine(t *testing.T) {
	scratchState(t)

	tun, err := StartForwarding()
	if err != nil {
		t.Fatal(err)
	}
	tun.Stop()
	tun.Stop()

	var never *Tunnels
	never.Stop()
	(&Tunnels{}).Stop()
}

// The core has to be pointed somewhere before any of this means anything; a
// store with no directory is the shape a phone would hand in, and it is refused
// rather than filled in with ~/.forge.
func TestForwardingNeedsSomewhereToKeepItsState(t *testing.T) {
	swapState(t, t.TempDir()) // and restore the machine's afterwards
	keys, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	if err := Use(&memStore{cfg: &config.Config{}}, keys); err != nil {
		t.Fatalf("wiring the stores: %v", err)
	}

	if _, err := StartForwarding(); err == nil {
		t.Error("started tunnels for a device with nowhere to keep their state")
	}
}
