package forge

import (
	"fmt"
	"sync"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/sshx"
	"github.com/Marb-AI/forge/keys"
)

// Where this device's state lives, and how the core is told.
//
// Until now every operation resolved it for itself: thirty-odd calls to a function
// that read $HOME and appended ".forge". That is a laptop's answer, and Forge is
// about to run where it is wrong — an iOS app has a container and no home
// directory, an Android app has its own directories, and a desktop shell may want
// to point a test build somewhere else entirely. One assumption in thirty places
// cannot be changed; a seam can.
//
// So the core holds two stores — the client config and this device's key — and
// every operation goes through them. A front end that has an opinion says so once,
// with Open or Use, before it asks for anything. One that does not gets ~/.forge,
// which is what every Forge in existence already had.
//
// This is process-wide state, deliberately: the core is a package of operations,
// not an object, and Forge is one core per process. Set it at startup, before the
// first operation — changing it under a running daemon would leave the tunnels it
// started answering to a config nobody is reading.
var (
	stateMu  sync.Mutex
	cfgStore config.Store
	keyStore keys.Store
)

// Open points the core at one directory on this machine: the config, the device
// key, the servers it trusts and the daemons' files all live in it. This is what
// a desktop shell calls, and what `forge` itself does by default with ~/.forge.
func Open(dir string) {
	// Cannot fail: both stores below are real ones.
	_ = Use(config.NewFileStore(dir), keys.NewFileStore(dir))
}

// Use points the core at stores of the caller's own — a phone's container, a
// keychain, a test's scratch directory. Both are given at once because a front end
// that knows where one belongs knows where the other does.
//
// A nil store is refused rather than accepted and worked around. Half a wiring is
// the one mistake this seam must not swallow: the core would fill the gap with
// ~/.forge and carry on, so a phone that got its config store wired and its keys
// wrong would look like it was working, and write a key to a path that means
// nothing there. The error arrives while there is still a front end to fix.
func Use(cfg config.Store, k keys.Store) error {
	if cfg == nil || k == nil {
		return fmt.Errorf("forge.Use needs both stores (config %v, keys %v)",
			present(cfg == nil), present(k == nil))
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	setStores(cfg, k)
	return nil
}

// setStores records the pair and points the transport at both halves: the
// directory where the servers this device has accepted are kept, and the key it
// reaches them with. Called under stateMu.
//
// The store's own Dir is handed over rather than the path it returns: resolving
// it can create the directory, and a Forge that never connects to anything
// should not leave one behind. It is resolved when a connection needs it, which
// is always after this — reaching a host means asking the config where it is.
func setStores(cfg config.Store, k keys.Store) {
	cfgStore, keyStore = cfg, k
	sshx.KnownHostsIn(cfg.Dir)
	// And the key it reaches them with. Handed over as the store's own method, so
	// it is read at the moment a connection needs it — a key made by `forge setup`
	// in another terminal is usable by a daemon that was already running.
	sshx.IdentityFrom(k.PrivateKey)
	// The same key as a path, when this store has one. Only the exec'd ssh needs
	// it, and only a store backed by files can answer — asked for rather than
	// assumed, because a Keychain cannot.
	if f, ok := k.(interface{ Path() string }); ok {
		sshx.IdentityFileFrom(func() (string, error) { return f.Path(), nil })
	} else {
		sshx.IdentityFileFrom(nil)
	}
}

// present names which half of the wiring was missing, so the error says what to
// go and fix.
func present(missing bool) string {
	if missing {
		return "missing"
	}
	return "given"
}

// Store returns the client config's store, defaulting to ~/.forge if no front end
// said otherwise.
func Store() (config.Store, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := useDefaults(); err != nil {
		return nil, err
	}
	return cfgStore, nil
}

// Keys returns this device's key store, defaulting alongside the config.
//
// Nothing in the core asks for it yet: Forge still reaches its servers by running
// `ssh`, which finds its own keys. The store is here so that the client which
// replaces it has a key to be handed, and so the answer to "where is it kept" is
// given in the same place as the rest of this device's state rather than invented
// separately later.
func Keys() (keys.Store, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if err := useDefaults(); err != nil {
		return nil, err
	}
	return keyStore, nil
}

// StateDir returns the directory the daemons keep their files in — pidfiles, logs,
// the UI token — creating it if necessary.
func StateDir() (string, error) {
	st, err := Store()
	if err != nil {
		return "", err
	}
	return st.Dir()
}

// useDefaults fills in the ~/.forge stores on first use. Called under stateMu.
//
// It is lazy rather than done in an init, because resolving a home directory can
// fail and an operation that never needed one should not be stopped by that. It is
// also the ONLY path in the core that reaches for $HOME at all.
// Both halves are checked, though Use sets both or neither: the pair is the
// invariant, and a check that only reads one of them stops saying so the day that
// changes.
func useDefaults() error {
	if cfgStore != nil && keyStore != nil {
		return nil
	}
	dir, err := config.DefaultDir()
	if err != nil {
		return err
	}
	setStores(config.NewFileStore(dir), keys.NewFileStore(dir))
	return nil
}

// loadConfig and updateConfig are what the operations use. They are the same two
// calls the config package used to export, with the store in the middle: read the
// current state, or change it as one atomic step.
func loadConfig() (*config.Config, error) {
	st, err := Store()
	if err != nil {
		return nil, err
	}
	return st.Load()
}

func updateConfig(change func(*config.Config) error) error {
	st, err := Store()
	if err != nil {
		return err
	}
	return st.Update(change)
}
