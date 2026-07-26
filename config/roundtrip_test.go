package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	c, err := NewFileStore(t.TempDir()).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 0 || len(c.Ports) != 0 || len(c.Workspaces) != 0 {
		t.Fatalf("expected empty config, got %+v", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir)

	c, _ := s.Load()
	c.Hosts["myserver"] = &Host{Alias: "myserver", User: "root", Addr: "1.2.3.4", Port: 22}
	c.AddWorkspace("crm", "myserver")
	c.Ports["myserver"] = map[string][]int{"crm": {3000, 5173}}
	if err := s.save(c); err != nil {
		t.Fatal(err)
	}

	// In the store's own directory, and nowhere else.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hosts["myserver"].Addr != "1.2.3.4" {
		t.Errorf("host not persisted: %+v", got.Hosts)
	}
	if got.HostFor("crm") == nil || got.HostFor("crm").Alias != "myserver" {
		t.Errorf("workspace->host not persisted")
	}
	if ports := got.Ports["myserver"]["crm"]; len(ports) != 2 || ports[0] != 3000 {
		t.Errorf("ports not persisted: %v", ports)
	}
}

func TestUIPortRoundTripAndDefault(t *testing.T) {
	s := NewFileStore(t.TempDir())

	// Unset means "use the default" — never port 0.
	c, _ := s.Load()
	if c.UIPort != 0 {
		t.Errorf("fresh config should have no explicit UI port, got %d", c.UIPort)
	}
	if c.UIPortOr() != DefaultUIPort {
		t.Errorf("UIPortOr() = %d, want the default %d", c.UIPortOr(), DefaultUIPort)
	}

	c.UIPort = 8099
	if err := s.save(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.UIPort != 8099 || got.UIPortOr() != 8099 {
		t.Errorf("UI port not persisted: got %d (UIPortOr %d)", got.UIPort, got.UIPortOr())
	}
}

// A config written by an older forge (no ui_port key) must still load, and fall
// back to the default rather than to port 0.
func TestOldConfigWithoutUIPortLoads(t *testing.T) {
	dir := t.TempDir()
	old := `{"hosts":{},"forwards":{},"workspaces":{}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := NewFileStore(dir).Load()
	if err != nil {
		t.Fatalf("an older config must still load: %v", err)
	}
	if c.UIPortOr() != DefaultUIPort {
		t.Errorf("UIPortOr() = %d, want the default %d", c.UIPortOr(), DefaultUIPort)
	}
}

// A config file can outlive the field it was written for: the prompts panel was
// dropped and `prompts` went with it, so every config saved while it existed now
// carries a key nothing here knows. It must load anyway — a key we no longer
// understand is not a broken config, and refusing it would lock someone out of
// their hosts over a feature they never used.
func TestConfigWithARetiredFieldStillLoads(t *testing.T) {
	dir := t.TempDir()
	old := `{"hosts":{"srv":{"alias":"srv","user":"root","addr":"10.0.0.1","port":22}},` +
		`"forwards":{},"workspaces":{"api":"srv"},"ui_port":8099,` +
		`"prompts":[{"id":"a1","title":"review the diff","text":"Review the diff."}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewFileStore(dir).Load()
	if err != nil {
		t.Fatalf("a config carrying a retired field must still load: %v", err)
	}
	// And everything beside it survives the trip, which is the actual worry: an
	// unknown key must be ignored, not swallow the fields around it.
	if c.Hosts["srv"] == nil || c.Workspaces["api"] != "srv" || c.UIPort != 8099 {
		t.Errorf("the rest of the config did not survive: %+v", c)
	}
}

// Every mutation is load, change, save — and it is the gap between the load and
// the save that loses data. Two of them interleaved each read the same file, and
// the second save writes back a copy that never saw the first one's change.
//
// The UI daemon runs all of them: a workspace being created in one tab while
// another registers a server, while a third sets the UI port. Update is what
// makes each of those one atomic step.
func TestConcurrentUpdatesDoNotLoseChanges(t *testing.T) {
	s := NewFileStore(t.TempDir())

	// Deliberately different FIELDS, which is the case a per-feature lock misses:
	// serialising workspace writes against each other does nothing about one of
	// them racing a host being registered.
	const writers = 8
	var wg sync.WaitGroup
	wg.Add(2 * writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			if err := s.Update(func(c *Config) error {
				c.Workspaces["w"+strconv.Itoa(i)] = "h"
				return nil
			}); err != nil {
				t.Errorf("update workspaces: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := s.Update(func(c *Config) error {
				c.Hosts["h"+strconv.Itoa(i)] = &Host{Alias: "h" + strconv.Itoa(i)}
				return nil
			}); err != nil {
				t.Errorf("update hosts: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != writers {
		t.Errorf("%d workspaces survived %d concurrent writes, want %d", len(got.Workspaces), writers, writers)
	}
	if len(got.Hosts) != writers {
		t.Errorf("%d hosts survived %d concurrent writes, want %d", len(got.Hosts), writers, writers)
	}
}

// A change that fails must leave the file alone: half-applying it would be worse
// than not applying it at all.
func TestUpdateDoesNotSaveWhenTheChangeFails(t *testing.T) {
	s := NewFileStore(t.TempDir())

	if err := s.Update(func(c *Config) error { c.UIPort = 8099; return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("no")
	if err := s.Update(func(c *Config) error {
		c.UIPort = 1234 // must not survive
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Update should return the change's error, got %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.UIPort != 8099 {
		t.Errorf("UI port = %d, want the failed change rolled back to 8099", got.UIPort)
	}
}

func TestHostForUnknown(t *testing.T) {
	c := &Config{Hosts: map[string]*Host{}, Workspaces: map[string]string{}}
	if c.HostFor("nope") != nil {
		t.Error("expected nil for unknown workspace")
	}
}
