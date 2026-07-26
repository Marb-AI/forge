package keys

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNoKeyUntilCreated(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "state"))

	if _, err := s.PublicKey(); !errors.Is(err, ErrNoKey) {
		t.Errorf("PublicKey on a fresh device = %v; want ErrNoKey", err)
	}
	if _, err := s.PrivateKey(); !errors.Is(err, ErrNoKey) {
		t.Errorf("PrivateKey on a fresh device = %v; want ErrNoKey", err)
	}

	created, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("Create on a fresh device reported no key was made")
	}
	pub, err := s.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5") {
		// The prefix is the wire encoding of the algorithm name, so it is the same
		// for every Ed25519 key ever written — a line that does not start with it is
		// not one sshd would accept.
		t.Errorf("public key = %q; want an ssh-ed25519 authorized_keys line", pub)
	}
	if strings.Count(pub, "\n") != 0 {
		t.Errorf("public key spans lines: %q", pub)
	}
}

// A second Create must not replace the key. Servers trust the one they were given;
// silently making another would lock the device out of every host at once.
func TestCreateKeepsAnExistingKey(t *testing.T) {
	s := NewFileStore(t.TempDir())
	if _, err := s.Create(); err != nil {
		t.Fatal(err)
	}
	before, err := s.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	created, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second Create reported it made a key")
	}
	after, err := s.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the key was replaced")
	}
}

// Two Creates at once must not each write a key. Looking first and writing second
// leaves a window where both see nothing — `forge setup` in two terminals, or the
// UI and a shell — and the loser's servers would stop letting it in.
func TestConcurrentCreatesMakeOneKey(t *testing.T) {
	s := NewFileStore(t.TempDir())

	const racers = 8
	var wg sync.WaitGroup
	created := make(chan bool, racers)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			made, err := s.Create()
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			created <- made
		}()
	}
	wg.Wait()
	close(created)

	made := 0
	for c := range created {
		if c {
			made++
		}
	}
	if made != 1 {
		t.Errorf("%d of %d Creates reported making a key; want exactly 1", made, racers)
	}
	// One key, and it is readable — not the last writer's half of two.
	if _, err := s.PublicKey(); err != nil {
		t.Errorf("the key that survived the race is not usable: %v", err)
	}
}

// A key file this store did not write must not be published as if it had. The
// line would name ssh-ed25519 over a blob that says otherwise — something no sshd
// accepts, discovered only after it had been installed on a server.
func TestPublicKeyRefusesAnotherKeyType(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen to make one with")
	}
	dir := t.TempDir()
	out, err := exec.Command(keygen, "-q", "-t", "ecdsa", "-N", "", "-C", "", "-f", filepath.Join(dir, "id.pem")).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen -t ecdsa: %v: %s", err, out)
	}

	s := NewFileStore(dir)
	pub, err := s.PublicKey()
	if err == nil {
		t.Fatalf("PublicKey on an ECDSA key = %q; want it refused", pub)
	}
	if !strings.Contains(err.Error(), "ecdsa") {
		t.Errorf("error = %v; it should name what the file actually holds", err)
	}
	// Creating over it must still be refused, key type or not: it is somebody's key.
	if made, err := s.Create(); err != nil || made {
		t.Errorf("Create over an existing key = %v, %v; want no key made", made, err)
	}
}

// The private key is the whole identity: a mode that lets another local user read
// it hands them every server this device can reach.
func TestPrivateKeyIsNotReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)
	if _, err := s.Create(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "id.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %04o; want 0600", perm)
	}
}

// The authorized_keys line is hand-encoded here, so the check that matters is
// whether OpenSSH derives the same one from the same private key. If it does not,
// the line does not authenticate anywhere and nothing else in this package would
// have noticed.
func TestPublicKeyMatchesOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen to check against")
	}
	dir := t.TempDir()
	s := NewFileStore(dir)
	if _, err := s.Create(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(keygen, "-y", "-f", filepath.Join(dir, "id.pem")).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v", err)
	}
	// ssh-keygen prints "<algo> <base64>" with no comment; compare those two fields.
	want := strings.Fields(strings.TrimSpace(string(out)))
	pub, err := s.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(pub)
	if len(got) != 3 || got[2] != keyComment {
		t.Errorf("public key = %q; want a line ending in the %q comment", pub, keyComment)
	}
	if len(want) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("public key = %q; ssh-keygen says %q", strings.Join(got[:2], " "), strings.Join(want, " "))
	}
}
