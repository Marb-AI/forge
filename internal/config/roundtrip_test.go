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
	t.Setenv("HOME", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 0 || len(c.Ports) != 0 || len(c.Workspaces) != 0 {
		t.Fatalf("expected empty config, got %+v", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c, _ := Load()
	c.Hosts["myserver"] = &Host{Alias: "myserver", User: "root", Addr: "1.2.3.4", Port: 22}
	c.AddWorkspace("crm", "myserver")
	c.Ports["myserver"] = map[string][]int{"crm": {3000, 5173}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	// File exists under ~/.forge.
	if _, err := os.Stat(filepath.Join(home, ".forge", "config.json")); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	got, err := Load()
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
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Unset means "use the default" — never port 0.
	c, _ := Load()
	if c.UIPort != 0 {
		t.Errorf("fresh config should have no explicit UI port, got %d", c.UIPort)
	}
	if c.UIPortOr() != DefaultUIPort {
		t.Errorf("UIPortOr() = %d, want the default %d", c.UIPortOr(), DefaultUIPort)
	}

	c.UIPort = 8099
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"hosts":{},"forwards":{},"workspaces":{}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("an older config must still load: %v", err)
	}
	if c.UIPortOr() != DefaultUIPort {
		t.Errorf("UIPortOr() = %d, want the default %d", c.UIPortOr(), DefaultUIPort)
	}
}

// Saved prompts are the one thing in here the user typed by hand, so losing them
// to a round trip is losing work — and their ORDER is theirs too, which is why
// they are a slice and not a map.
func TestPromptsRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c, _ := Load()
	if len(c.Prompts) != 0 {
		t.Fatalf("a fresh config should hold no prompts, got %+v", c.Prompts)
	}
	c.Prompts = []Prompt{
		{ID: "a1", Title: "review the diff", Text: "Review the diff and tell me\nwhat you'd change."},
		{ID: "b2", Title: "run the tests", Text: "Run the tests and fix what breaks."},
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Prompts) != 2 {
		t.Fatalf("prompts not persisted: %+v", got.Prompts)
	}
	if got.Prompts[0].ID != "a1" || got.Prompts[1].ID != "b2" {
		t.Errorf("prompt order not preserved: %+v", got.Prompts)
	}
	// A prompt is usually several lines; a round trip that flattened one would
	// change what gets typed into the session.
	if got.Prompts[0].Text != "Review the diff and tell me\nwhat you'd change." {
		t.Errorf("prompt text not preserved verbatim: %q", got.Prompts[0].Text)
	}
}

// A config written by a forge that had no prompts must still load — and come
// back with an empty library, not an error.
func TestOldConfigWithoutPromptsLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := `{"hosts":{},"forwards":{},"workspaces":{},"ui_port":8099}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("an older config must still load: %v", err)
	}
	if len(c.Prompts) != 0 {
		t.Errorf("expected no prompts, got %+v", c.Prompts)
	}
}

// Every mutation is load, change, save — and it is the gap between the load and
// the save that loses data. Two of them interleaved each read the same file, and
// the second save writes back a copy that never saw the first one's change.
//
// The UI daemon runs all of them: a prompt saved in one tab while another sets
// the UI port, while a third finishes creating a workspace. Update is what makes
// each of those one atomic step.
func TestConcurrentUpdatesDoNotLoseChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Deliberately different FIELDS, which is the case a per-feature lock misses:
	// serialising prompt writes against each other does nothing about a prompt
	// write racing the UI port.
	const writers = 8
	var wg sync.WaitGroup
	wg.Add(2 * writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			if err := Update(func(c *Config) error {
				c.Prompts = append(c.Prompts, Prompt{ID: strconv.Itoa(i), Title: "t", Text: "x"})
				return nil
			}); err != nil {
				t.Errorf("update prompts: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := Update(func(c *Config) error {
				c.Hosts["h"+strconv.Itoa(i)] = &Host{Alias: "h" + strconv.Itoa(i)}
				return nil
			}); err != nil {
				t.Errorf("update hosts: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Prompts) != writers {
		t.Errorf("%d prompts survived %d concurrent writes, want %d", len(got.Prompts), writers, writers)
	}
	if len(got.Hosts) != writers {
		t.Errorf("%d hosts survived %d concurrent writes, want %d", len(got.Hosts), writers, writers)
	}
}

// A change that fails must leave the file alone: half-applying it would be worse
// than not applying it at all.
func TestUpdateDoesNotSaveWhenTheChangeFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Update(func(c *Config) error { c.UIPort = 8099; return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("no")
	if err := Update(func(c *Config) error {
		c.UIPort = 1234 // must not survive
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Update should return the change's error, got %v", err)
	}
	got, err := Load()
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
