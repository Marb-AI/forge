// Package keys holds the SSH identity a device reaches its servers with.
//
// It exists as a seam before it has a user. Today Forge runs `ssh`, which finds
// its own keys in ~/.ssh; the pure-Go client that replaces it has to be handed a
// key instead, and where that key is kept is exactly the decision that cannot be
// hardcoded — on a laptop it is a file, on iOS the Keychain, on Android the
// Keystore, and on hardware-backed keys the private half never leaves the chip and
// cannot be read at all. A Store is what those have in common: it can be asked to
// make a key, and asked for the public half to install on a server.
//
// FileStore is the answer for a machine with a filesystem, and the only one Forge
// ships. Nothing calls it yet — the key becomes real when the SSH client that uses
// it does.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// ErrNoKey is what a store answers when this device has no key yet. It is a state
// to offer a fix for, not a failure: a fresh install has no key, and the first
// thing setup does is make one.
var ErrNoKey = errors.New("this device has no key yet")

// Store is one device's SSH identity.
//
// Create and PublicKey are separate because the product separates them: the key is
// made once, deliberately, and its public half is then shown as many times as you
// need it — pasted into a new server's cloud-init, or installed on one that is
// already running. Nothing creates a key as a side effect of being asked for one.
type Store interface {
	// Create generates this device's key if it does not have one, and reports
	// whether it made one. Calling it again is a no-op: a key that servers already
	// trust must not be replaced by a surprise.
	Create() (created bool, err error)
	// PublicKey returns the key in authorized_keys form — the single line that goes
	// into cloud-init or onto a server. ErrNoKey if there is none.
	PublicKey() (string, error)
	// PrivateKey returns the key material in PEM form, for an SSH client to parse.
	// ErrNoKey if there is none.
	//
	// A hardware-backed store cannot answer this and will not implement it this way;
	// that is a signing callback, and the day Forge has one this interface grows a
	// Signer instead. Naming it now would be guessing at its shape.
	PrivateKey() ([]byte, error)
}

// keyComment is what the public key line is tagged with. It says which program
// put the key there, on a server whose authorized_keys may hold several.
const keyComment = "forge"

// FileStore keeps the key in a directory on this machine, as one PEM file.
type FileStore struct{ dir string }

// NewFileStore returns a Store backed by dir, which is created on first write.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// path is where the private key lives. The public half is derived from it rather
// than stored: two files that can disagree are a bug waiting for someone to edit
// one of them.
func (s *FileStore) path() string { return filepath.Join(s.dir, "id.pem") }

// Path is that, for the one caller that needs a file rather than bytes: the ssh
// binary, which takes -i and cannot be handed a key any other way.
//
// Deliberately not on Store. A store that keeps the key in a Keychain, or on a
// chip it never leaves, has no path to give — and the only thing that wants one
// is a command line, which exists only where there is a filesystem to run it on.
// Whoever needs it asks whether the store has it.
func (s *FileStore) Path() string { return s.path() }

// Create generates an Ed25519 key. Ed25519 because it is the modern default
// everywhere Forge connects and the keys are short enough to paste; the eventual
// hardware-backed key will be P-256 instead, because Secure Enclave does not do
// Ed25519 — which is a different store, not a different file format here.
func (s *FileStore) Create() (bool, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false, err
	}
	// O_EXCL is what makes "keep the existing key" a guarantee rather than a
	// likelihood: looking first and writing second leaves a window in which the two
	// halves of `forge setup` in two terminals — or the UI and a shell — each see no
	// key and each write one, and the loser's servers stop letting it in. The kernel
	// decides instead, and the one that loses the create reports what is true: it
	// made no key.
	//
	// 0600 from the moment the file exists, rather than chmod'ed after: the gap
	// between the two is a window where the key is readable.
	f, err := os.OpenFile(s.path(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(marshalOpenSSH(pub, priv)); err != nil {
		f.Close()
		// A half-written key is worse than none: it is not a key, and it would make
		// every later Create report that this device already has one.
		os.Remove(s.path())
		return false, err
	}
	return true, f.Close()
}

// PrivateKey returns the PEM as written.
func (s *FileStore) PrivateKey() ([]byte, error) {
	data, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// PublicKey derives the authorized_keys line from the stored key, rather than
// keeping it in a second file — two files that can disagree are a bug waiting for
// someone to edit one of them.
func (s *FileStore) PublicKey() (string, error) {
	pemBytes, err := s.PrivateKey()
	if err != nil {
		return "", err
	}
	algo, blob, err := publicBlob(pemBytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", s.path(), err)
	}
	// The algorithm is read out of the file rather than assumed. A line that says
	// ssh-ed25519 over a blob that says anything else is not a key sshd will ever
	// accept, and it would be installed on a server before anyone found out — so a
	// file this store did not write is an error here, at the one moment there is
	// still something to do about it.
	if algo != keyAlgo {
		return "", fmt.Errorf("%s: holds a %q key, and this store only writes %s",
			s.path(), algo, keyAlgo)
	}
	// authorized_keys is the algorithm name, the base64 of that same wire blob the
	// file already carries, and a comment.
	return fmt.Sprintf("%s %s %s", algo, base64.StdEncoding.EncodeToString(blob), keyComment), nil
}

// The OpenSSH private key format, written by hand.
//
// OpenSSH is the only reader that matters here — `ssh -i` must accept the file,
// which rules out the PEM the standard library writes, since OpenSSH dropped
// OpenSSL's container for Ed25519 entirely. The format itself is a handful of
// length-prefixed strings, so the choice is between encoding those and taking a
// dependency on an SSH library to concatenate them for us. The test checks the
// result against ssh-keygen, which is what makes writing it out defensible: if a
// field is wrong, the key does not authenticate anywhere and the test says so.
const (
	keyAlgo = "ssh-ed25519"
	// pemType and magic are the format's own markers: the armour OpenSSH looks for
	// and the header inside it.
	pemType = "OPENSSH PRIVATE KEY"
	magic   = "openssh-key-v1\x00"
	// blockSize is the "none" cipher's, and what the private section is padded to.
	blockSize = 8
)

// marshalOpenSSH renders an unencrypted key file: the header, the public key, and
// a section that a real cipher would have encrypted.
func marshalOpenSSH(pub ed25519.PublicKey, priv ed25519.PrivateKey) []byte {
	blob := sshString(nil, []byte(keyAlgo))
	blob = sshString(blob, pub)

	// checkint, twice. Decrypting with the wrong passphrase yields two halves that
	// do not match, which is how OpenSSH tells a bad passphrase from a corrupt file.
	// There is no passphrase here, so any value does — but it must still be written
	// twice, and the same twice.
	check := make([]byte, 4)
	if _, err := rand.Read(check); err != nil {
		// A failing system RNG has already broken key generation above; this is only
		// a tag, so there is nothing here worth failing the write for.
		check = []byte{0, 0, 0, 0}
	}
	inner := append(append([]byte{}, check...), check...)
	inner = sshString(inner, []byte(keyAlgo))
	inner = sshString(inner, pub)
	inner = sshString(inner, priv) // seed and public half, as OpenSSH stores it
	inner = sshString(inner, []byte(keyComment))
	// Padding is 1, 2, 3… so a decoder can tell padding from data.
	for i := byte(1); len(inner)%blockSize != 0; i++ {
		inner = append(inner, i)
	}

	out := []byte(magic)
	out = sshString(out, []byte("none")) // cipher
	out = sshString(out, []byte("none")) // kdf
	out = sshString(out, nil)            // kdf options
	out = binary.BigEndian.AppendUint32(out, 1)
	out = sshString(out, blob)
	out = sshString(out, inner)
	return pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: out})
}

// publicBlob reads the public key back out of a key file: the wire blob
// authorized_keys carries base64'd, and the algorithm the blob itself declares.
func publicBlob(pemBytes []byte) (algo string, blob []byte, err error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != pemType {
		return "", nil, errors.New("not an OpenSSH private key")
	}
	rest, ok := bytes.CutPrefix(block.Bytes, []byte(magic))
	if !ok {
		return "", nil, errors.New("wrong key file header")
	}
	for range 3 { // cipher, kdf, kdf options
		if _, rest, ok = sshField(rest); !ok {
			return "", nil, errors.New("truncated key file")
		}
	}
	if len(rest) < 4 {
		return "", nil, errors.New("truncated key file")
	}
	rest = rest[4:] // number of keys, always 1 for a file Forge wrote
	blob, _, ok = sshField(rest)
	if !ok {
		return "", nil, errors.New("truncated key file")
	}
	// The blob's own first field is the algorithm name, which is what makes it
	// self-describing — and what lets the caller check it against the key this
	// store believes it wrote.
	name, _, ok := sshField(blob)
	if !ok {
		return "", nil, errors.New("public key names no algorithm")
	}
	return string(name), blob, nil
}

// sshString appends one length-prefixed field, RFC 4253's encoding for all of
// this.
func sshString(dst, val []byte) []byte {
	if len(val) > math.MaxUint32 {
		panic("ssh string too long") // unreachable: every field here is a key or a name
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(val)))
	return append(dst, val...)
}

// sshField reads one back, returning it and what follows.
func sshField(src []byte) (val, rest []byte, ok bool) {
	if len(src) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(src[:4])
	if uint64(len(src)-4) < uint64(n) {
		return nil, nil, false
	}
	return src[4 : 4+n], src[4+n:], true
}
