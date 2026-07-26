package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The store is only worth having if two of them are genuinely separate: that is
// what a phone's container, or a desktop shell pointed at a test directory, buys.
func TestStoresAreIndependent(t *testing.T) {
	a, b := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())

	if err := a.Update(func(c *Config) error {
		c.Hosts["srv"] = &Host{Alias: "srv"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := b.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hosts) != 0 {
		t.Errorf("the second store saw the first one's hosts: %v", got.Hosts)
	}
}

// A store is handed a directory that need not exist yet — a fresh install has no
// ~/.forge, and neither does a new app container.
func TestFileStoreCreatesItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "forge")
	s := NewFileStore(dir)

	got, err := s.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("Dir() = %q, want %q", got, dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The config holds every server this client can reach. Another local user has
	// no business reading the list, let alone writing one in.
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("state directory mode = %04o; want 0700", perm)
	}
}

// ~/.forge is still where a laptop keeps its state — the seam moves the decision,
// it does not move the directory.
func TestDefaultDirIsForgeUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows

	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".forge"); dir != want {
		t.Errorf("DefaultDir() = %q, want %q", dir, want)
	}
	// Resolving it must not create it: asking where the state would live is not the
	// same as declaring that this device has some.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("DefaultDir() created the directory (stat: %v)", err)
	}
}
