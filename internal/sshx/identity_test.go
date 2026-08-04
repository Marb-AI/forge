package sshx

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// What the transport says when it has no key to offer. All three are told apart
// on purpose: they are three different things to go and do, and an
// authentication failure that says none of them is how you end up reading a
// handshake trace to find out you never ran setup.
func TestTheTransportSaysWhichKeyIsMissing(t *testing.T) {
	t.Run("nothing wired it", func(t *testing.T) {
		prev := identityFn
		IdentityFrom(nil)
		t.Cleanup(func() { IdentityFrom(prev) })

		_, err := identity()
		if !errors.Is(err, errNoIdentity) {
			t.Errorf("err = %v, want it to say nothing pointed the transport at a key", err)
		}
	})

	t.Run("the device has no key yet", func(t *testing.T) {
		absent := errors.New("this device has no key yet")
		prev := identityFn
		IdentityFrom(func() ([]byte, error) { return nil, absent })
		t.Cleanup(func() { IdentityFrom(prev) })

		_, err := identity()
		if !errors.Is(err, absent) {
			t.Errorf("err = %v, want the store's own words kept", err)
		}
		// The one thing to do about it, in the error itself.
		if !strings.Contains(err.Error(), "forge setup") {
			t.Errorf("err = %v, want it to name the command that makes one", err)
		}
	})

	t.Run("the key cannot be parsed", func(t *testing.T) {
		useIdentity(t, []byte("this is not a key"))
		if _, err := identity(); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("err = %v, want it to say the key is unreadable", err)
		}
	})
}

// The key is read on every dial rather than parsed once and held: `forge setup`
// run in another terminal has to reach a UI daemon that was already up, without
// restarting it.
func TestTheKeyIsReadWhenItIsNeeded(t *testing.T) {
	reads := 0
	prev := identityFn
	IdentityFrom(func() ([]byte, error) {
		reads++
		return nil, errors.New("no key")
	})
	t.Cleanup(func() { IdentityFrom(prev) })

	identity()
	identity()
	if reads != 2 {
		t.Errorf("the store was asked %d times for two dials — a key made since the "+
			"first one would never be seen", reads)
	}
}

// And it is that key the server sees. The rest of this package's tests rely on
// it, but nothing else says it outright: a server that accepts only the device's
// public half lets this client in.
func TestTheServerSeesTheDeviceKey(t *testing.T) {
	pub := writeClientKey(t)
	srv := startServer(t, pub, func(string, io.Reader) (string, string, int) {
		return "in", "", 0
	})
	trust(t, srv)
	useGo(t)

	out, err := srv.target("crm").Output("id")
	if err != nil {
		t.Fatalf("the server did not accept this device's key: %v", err)
	}
	if string(out) != "in" {
		t.Errorf("Output = %q", out)
	}
}

// Nothing in the transport looks for a key on this machine any more. The names
// are the giveaway: an identity file walk is how it used to find one.
func TestTheTransportLooksForNoKeyOfItsOwn(t *testing.T) {
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "SSH_AUTH_SOCK"} {
		for _, file := range []string{"gossh.go", "identity.go"} {
			if strings.Contains(sourceOf(t, file), name) && !(file == "gossh.go" && name == "SSH_AUTH_SOCK") {
				t.Errorf("%s still names %q — the device key is handed in, not looked for", file, name)
			}
		}
	}
}

// sourceOf reads one of this package's files.
func sourceOf(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
