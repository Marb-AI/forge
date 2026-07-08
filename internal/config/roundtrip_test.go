package config

import (
	"os"
	"path/filepath"
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
	c.SetPorts("myserver", "crm", []int{3000, 5173})
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

func TestSetPortsEmptyRemoves(t *testing.T) {
	c := &Config{Ports: map[string]map[string][]int{}}
	c.SetPorts("h", "w", []int{1, 2})
	c.SetPorts("h", "w", nil) // empty removes
	if len(c.Ports) != 0 {
		t.Errorf("expected host pruned, got %v", c.Ports)
	}
}

func TestHostForUnknown(t *testing.T) {
	c := &Config{Hosts: map[string]*Host{}, Workspaces: map[string]string{}}
	if c.HostFor("nope") != nil {
		t.Error("expected nil for unknown workspace")
	}
}
