package agent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const aKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIsecond forge@phone"

// noChown stands in for the real one: these accounts do not exist on a machine
// running tests, and what is being tested is which files are written, not that
// the OS can chown to a user it has never heard of. It records what it was asked
// for, so the *choice* of account is still checked.
func noChown(t *testing.T) *map[string]string {
	t.Helper()
	seen := map[string]string{}
	prev := chownTo
	chownTo = func(owner, dir string) error {
		seen[dir] = owner
		return nil
	}
	t.Cleanup(func() { chownTo = prev })
	return &seen
}

func authorized(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// A key in the host login alone is a Forge that lists everything and opens
// nothing: the admin account is how a host is asked anything, and the workspaces
// are where a session, a shell and every tunnelled port actually are.
func TestAuthorisingReachesTheWorkspacesToo(t *testing.T) {
	scratchHost(t, "one", "two", "somebody-elses")
	for _, name := range []string{"one", "two"} {
		if err := recordWorkspace(name); err != nil {
			t.Fatal(err)
		}
	}
	admin := t.TempDir()
	t.Setenv("HOME", admin)
	t.Setenv("SUDO_USER", "")
	owners := noChown(t)

	out := captureStdout(t)
	if code := opAuthorizeKey([]string{"-key", base64.StdEncoding.EncodeToString([]byte(aKey))}); code != 0 {
		t.Fatalf("authorising exited %d: %s", code, out())
	}
	out()

	for _, name := range []string{"one", "two"} {
		if got := authorized(t, filepath.Join(baseDir, name)); !strings.Contains(got, aKey) {
			t.Errorf("workspace %q would not let the paired device in", name)
		}
	}
	// And an account Forge did not make is not ours to hand out access to,
	// whoever is asking.
	if got := authorized(t, filepath.Join(baseDir, "somebody-elses")); got != "" {
		t.Errorf("a workspace Forge never made was opened up: %q", got)
	}
	// Each workspace's .ssh ends up owned by that workspace, not by root. sshd
	// refuses a file the account does not own, so a key written without this is a
	// key that silently does not work.
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(baseDir, name, ".ssh")
		if got := (*owners)[dir]; got != name+":"+name {
			t.Errorf("%s would be owned by %q, want %q", dir, got, name+":"+name)
		}
	}
}

// The file is how everything else gets in, so it is appended to and never
// rewritten: the device already there must still be there afterwards.
func TestAuthorisingKeepsWhoeverWasAlreadyIn(t *testing.T) {
	scratchHost(t, "one")
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(baseDir, "one")
	first := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIfirst forge@laptop"
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"),
		[]byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	noChown(t)

	out := captureStdout(t)
	opAuthorizeKey([]string{"-key", base64.StdEncoding.EncodeToString([]byte(aKey))})
	out()

	got := authorized(t, home)
	if !strings.Contains(got, first) {
		t.Error("the device that was already in was locked out")
	}
	if !strings.Contains(got, aKey) {
		t.Error("the new device was not let in")
	}
}

// Pairing the same device twice is pairing it once. Without that, every repeat
// grows a file that decides who may log in.
func TestAuthorisingTwiceDoesNotGrowTheFile(t *testing.T) {
	scratchHost(t, "one")
	if err := recordWorkspace("one"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	noChown(t)

	enc := base64.StdEncoding.EncodeToString([]byte(aKey))
	for range 3 {
		out := captureStdout(t)
		if code := opAuthorizeKey([]string{"-key", enc}); code != 0 {
			t.Fatalf("authorising exited %d: %s", code, out())
		}
		out()
	}
	if n := strings.Count(authorized(t, filepath.Join(baseDir, "one")), aKey); n != 1 {
		t.Errorf("the key is in the file %d times", n)
	}
}

// This text is appended to a file that decides who may log in, so its shape is
// checked. A newline in it would authorise a second key nobody asked about, and
// that key could be anything.
func TestOnlyOneKeyOnOneLineIsAccepted(t *testing.T) {
	for _, bad := range []string{
		"",
		"not a key at all",
		aKey + "\nssh-ed25519 AAAAsomebodyelse attacker@theirs",
		aKey + "\rssh-ed25519 AAAAsomebodyelse attacker@theirs",
	} {
		scratchHost(t, "one")
		if err := recordWorkspace("one"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("SUDO_USER", "")

		out := captureStdout(t)
		code := opAuthorizeKey([]string{"-key", base64.StdEncoding.EncodeToString([]byte(bad))})
		said := out()

		if code == 0 {
			t.Errorf("%q was accepted as a key", bad)
		}
		if !strings.Contains(said, `"error"`) {
			t.Errorf("%q was refused without saying why: %s", bad, said)
		}
		if got := authorized(t, filepath.Join(baseDir, "one")); got != "" {
			t.Errorf("%q was written anyway: %q", bad, got)
		}
	}
}

// A workspace the record names and the host no longer has is skipped rather than
// failing the whole thing: the other workspaces still want the key, and a device
// half paired is a device that cannot be told which half.
func TestARecordedWorkspaceThatIsGoneDoesNotStopTheRest(t *testing.T) {
	scratchHost(t, "here")
	for _, name := range []string{"here", "vanished"} {
		if err := recordWorkspace(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	noChown(t)

	out := captureStdout(t)
	code := opAuthorizeKey([]string{"-key", base64.StdEncoding.EncodeToString([]byte(aKey))})
	said := out()

	if code != 0 {
		t.Fatalf("one missing workspace stopped the rest: %s", said)
	}
	if !strings.Contains(authorized(t, filepath.Join(baseDir, "here")), aKey) {
		t.Error("the workspace that is there was not opened")
	}
	if !strings.Contains(said, `"here"`) || strings.Contains(said, `"vanished"`) {
		t.Errorf("the answer names the wrong workspaces: %s", said)
	}
}
