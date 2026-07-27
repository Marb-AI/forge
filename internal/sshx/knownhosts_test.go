package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Trust-on-first-use, against the same real SSH server the rest of this
// backend is tested against. Nothing below could be shown with a fake: what is
// being checked is which key the server presented in a handshake, and what this
// device wrote down about it afterwards.

// TestTheGoClientTrustsAServerOnFirstSightAndWritesItDown is the policy the
// exec'd backend gets from StrictHostKeyChecking=accept-new, in Forge's own
// file: a server nobody has vouched for is accepted once, and never has to be
// again.
func TestTheGoClientTrustsAServerOnFirstSightAndWritesItDown(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "ok", "", 0
	})
	knownHostsPath(t) // ~/.ssh has a file, and this server is deliberately not in it
	dir := recordHostKeysIn(t)
	useGo(t)

	if _, err := srv.target("crm").Output("id"); err != nil {
		t.Fatalf("a server seen for the first time was refused: %v", err)
	}

	want := knownhosts.Line([]string{knownhosts.Normalize(srv.addr.String())}, srv.hostKey)
	if got := lines(t, filepath.Join(dir, "known_hosts")); len(got) != 1 || got[0] != want {
		t.Fatalf("recorded %q, want exactly one line: %q", got, want)
	}

	// And a second connection adds nothing: the host is known now, so there is
	// no first sight to record.
	if _, err := srv.target("crm").Output("id"); err != nil {
		t.Fatalf("a recorded server was refused on the next connection: %v", err)
	}
	if got := lines(t, filepath.Join(dir, "known_hosts")); len(got) != 1 {
		t.Errorf("the file holds %d lines after a second connection, want 1: %q", len(got), got)
	}
}

// A key that changed is the one thing trust-on-first-use exists to catch. It is
// refused, the message says enough to act on, and nothing overwrites the record
// — only the person reading it can tell a rebuilt server from an intercepted
// one, and it is their line to delete.
func TestTheGoClientRefusesAServerWhoseKeyChanged(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	dir := recordHostKeysIn(t)
	path := filepath.Join(dir, "known_hosts")
	stale := knownhosts.Line([]string{knownhosts.Normalize(srv.addr.String())}, otherHostKey(t))
	if err := os.WriteFile(path, []byte(stale+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	useGo(t)

	_, err := srv.target("crm").Output("id")
	if err == nil {
		t.Fatal("a server presenting a different key was accepted")
	}
	for _, want := range []string{"CHANGED", ssh.FingerprintSHA256(srv.hostKey), path, "line 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if got := lines(t, path); len(got) != 1 || got[0] != stale {
		t.Errorf("the record was rewritten to %q; the old key must stay until someone removes it", got)
	}
}

// The ssh binary's known_hosts is still read, so a host it already trusts is not
// trusted on sight a second time — and still never written, which is the half of
// this that moved. (It goes with the rest of the ~/.ssh borrowing in v2.)
func TestTheGoClientReadsOpenSSHsKnownHostsAndWritesNothingToIt(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	trust(t, srv) // recorded in ~/.ssh/known_hosts, as `ssh` would have
	before, err := os.ReadFile(knownHostsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := recordHostKeysIn(t)
	useGo(t)

	if _, err := srv.target("crm").Output("id"); err != nil {
		t.Fatalf("a host ssh already trusts was refused: %v", err)
	}

	if got := lines(t, filepath.Join(dir, "known_hosts")); len(got) != 0 {
		t.Errorf("copied %q into Forge's own file; a host that is already known is not a first sight", got)
	}
	after, err := os.ReadFile(knownHostsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("~/.ssh/known_hosts was written to; it is read-only to Forge")
	}
}

// Nothing having said where this device keeps its state is a wiring mistake, and
// the one place it shows is here — a host that cannot be recorded is a host that
// cannot be trusted on sight. It says so rather than failing as "key is unknown".
func TestTheGoClientRefusesAnUnknownServerWithNowhereToRecordIt(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "", "", 0
	})
	knownHostsPath(t) // the file exists; this server is deliberately not in it
	useGo(t)          // and nothing called KnownHostsIn

	_, err := srv.target("crm").Output("id")
	if err == nil {
		t.Fatal("an unknown server was accepted with nowhere to record it")
	}
	if !strings.Contains(err.Error(), "never been seen") ||
		!strings.Contains(err.Error(), errNoStateDir.Error()) {
		t.Errorf("error does not say why it could not be recorded: %v", err)
	}
}

// KnownHosts is a path, not a directory that springs into existence because
// something asked where it would be.
func TestAskingWhereTheHostKeysGoCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	prev := stateDir
	KnownHostsIn(func() (string, error) { return dir, nil })
	t.Cleanup(func() { KnownHostsIn(prev) })

	path, err := KnownHosts()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "known_hosts"); path != want {
		t.Errorf("KnownHosts() = %q, want %q", path, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the state directory was created just by asking: %v", err)
	}
}

// recordHostKeysIn points the transport at a directory of this test's own and
// returns it — the wiring the core does with its state directory.
func recordHostKeysIn(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := stateDir
	KnownHostsIn(func() (string, error) { return dir, nil })
	t.Cleanup(func() { KnownHostsIn(prev) })
	return dir
}

// otherHostKey is a host key that is not the test server's.
func otherHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// lines is the file's records, without the blank one a trailing newline leaves.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
