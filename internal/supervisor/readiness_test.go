package supervisor

import (
	"errors"
	"os"
	"testing"

	"github.com/Marb-AI/forge/config"
)

// The pidfile is a promise, and Run must not make it early.
//
// startSupervisor waits up to three seconds for a pidfile naming a live process
// and reports that as "the supervisor is up". Write it before the work that can
// fail and the answer becomes a guess: the daemon exits a moment later, the
// caller has already been told it started, and the first sign of trouble is
// tunnels that never appear.
//
// So the test asks the one question that pins the order — was the pidfile there
// yet, at the moment the config was read — rather than looking afterwards, when
// the deferred removal has tidied the evidence away either way.
func TestTheSupervisorClaimsThePidfileOnlyOnceItCannotFail(t *testing.T) {
	dir := t.TempDir()
	store := &watchingStore{dir: dir}

	err := Run(store, nil)
	if err == nil {
		t.Fatal("a config that cannot be read started a supervisor anyway")
	}
	if !store.read {
		t.Fatal("Run never read the config, so this proves nothing about the order")
	}
	if store.pidfileSeen {
		t.Error("the pidfile was already there when the config was read — a caller " +
			"waiting on it would call this daemon started, and it is about to exit")
	}
	if _, err := os.Stat(PIDPath(dir)); !os.IsNotExist(err) {
		t.Error("a supervisor that failed to start left its pidfile behind")
	}
}

// watchingStore fails to load, and remembers whether the pidfile existed when it
// was asked — which is the last moment before Run may legitimately write one.
type watchingStore struct {
	dir         string
	read        bool
	pidfileSeen bool
}

func (s *watchingStore) Dir() (string, error) { return s.dir, nil }

func (s *watchingStore) Load() (*config.Config, error) {
	s.read = true
	_, err := os.Stat(PIDPath(s.dir))
	s.pidfileSeen = err == nil
	return nil, errors.New("this config cannot be read")
}

func (s *watchingStore) Update(func(*config.Config) error) error {
	return errors.New("this config cannot be written")
}
