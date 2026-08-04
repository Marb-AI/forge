package sshx

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Which key this device reaches its servers with.
//
// It used to be whichever the machine already had — the agent's, or the first
// readable file in ~/.ssh. That is a laptop's answer to a question a phone
// cannot even hear: there is no agent there, no ~/.ssh, and no ssh-keygen that
// ever ran. And it was never really an answer here either, because "the key I
// happen to have" is not something Forge can promise a server anything about.
//
// So the key is handed in, the same way the state directory is: the core holds
// it and points the transport at it. The transport is given the key material
// rather than a signer, deliberately — the core's whole vocabulary is targets
// and commands, never keys or ports or ssh options (see this package's doc), and
// an ssh.Signer in forge/ would be the first crack in that. Parsing belongs down
// here, where ssh types already live.
//
// The day the key is in a Secure Enclave this becomes a signing callback instead,
// because there will be no bytes to hand over. That is a change to this seam, and
// this is the seam it changes.

var (
	identityMu sync.Mutex
	identityFn func() ([]byte, error)
	// identityPath is the same key as a file, for the one client that cannot be
	// handed bytes: the ssh binary, which takes -i. Empty on a device whose store
	// has no file to point at — a phone — where nothing execs ssh anyway.
	identityPath func() (string, error)
)

// errNoIdentity is nothing having pointed the transport at a key. Like
// errNoStateDir it is a wiring mistake rather than a user's problem: the core
// points here when it resolves its own stores.
var errNoIdentity = errors.New("nothing told the transport which key this device uses")

// IdentityFrom tells the transport where to get this device's private key. The
// core calls it with its key store.
func IdentityFrom(key func() ([]byte, error)) {
	identityMu.Lock()
	defer identityMu.Unlock()
	identityFn = key
}

// IdentityFileFrom tells it where that key is on disk, when it is on disk. Only
// the exec'd ssh needs this; everything else takes the bytes.
func IdentityFileFrom(path func() (string, error)) {
	identityMu.Lock()
	defer identityMu.Unlock()
	identityPath = path
}

// identityFile is the key's path, or empty when this device keeps it somewhere a
// path cannot describe. No error: a missing path is not a failure here, it is an
// argument that does not get added.
func identityFile() string {
	identityMu.Lock()
	path := identityPath
	identityMu.Unlock()
	if path == nil {
		return ""
	}
	p, err := path()
	if err != nil {
		return ""
	}
	return p
}

// identity is the signer every connection authenticates with.
//
// Read on each dial rather than parsed once and kept: a key made after this
// process started — `forge setup` in another terminal while the UI daemon is
// already up — has to be usable without restarting anything.
func identity() (ssh.Signer, error) {
	identityMu.Lock()
	key := identityFn
	identityMu.Unlock()
	if key == nil {
		return nil, errNoIdentity
	}
	pem, err := key()
	if err != nil {
		// The store's own words for "this device has no key yet", plus the one
		// thing to do about it. A transport naming a command is a small leak, and
		// cheaper than the alternative: an authentication failure that says
		// nothing about which key was missing or how to make one.
		return nil, fmt.Errorf("%w (run: forge setup)", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("this device's key cannot be read: %w", err)
	}
	return signer, nil
}
