package forge

import (
	"testing"

	"github.com/Marb-AI/forge/config"
)

// Registering a server, listing what is registered, and forgetting one: the
// whole of what a client knows about a host before it talks to it. It used to be
// written out inside the CLI's command, which is why it was tested by running
// argv through a dispatcher; it is an operation, and it is tested as one.
//
// The config it writes is the throwaway one this package's TestMain installs.
func TestAddHostListAndRemove(t *testing.T) {
	host, err := AddHost("root@1.2.3.4", "srv-add-test")
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	if host.User != "root" || host.Addr != "1.2.3.4" || host.Port != 22 {
		t.Errorf("added %+v, want root@1.2.3.4:22", host)
	}
	t.Cleanup(func() { _ = RemoveHost("srv-add-test") })

	// The duplicate check has to be inside the write, not before it: checked
	// against a config loaded earlier, two adds of one alias would both pass and
	// the second would silently replace the first — a server you think you
	// registered, pointing somewhere else.
	if _, err := AddHost("root@5.6.7.8", "srv-add-test"); err == nil {
		t.Error("adding an alias twice should be refused")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Hosts["srv-add-test"]; got == nil || got.Addr != "1.2.3.4" {
		t.Errorf("the refused add overwrote the original: %+v", got)
	}

	if _, err := AddHost("root@1.2.3.4:ssh", "bad-port"); err == nil {
		t.Error("an unparseable ssh target should be refused")
	}

	hosts, err := Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if !containsAlias(hosts, "srv-add-test") {
		t.Errorf("added host missing from Hosts(): %+v", hosts)
	}
	// Sorted, so the same set of servers reads the same way twice running.
	for i := 1; i < len(hosts); i++ {
		if hosts[i-1].Alias > hosts[i].Alias {
			t.Errorf("Hosts() is not sorted by alias: %q before %q", hosts[i-1].Alias, hosts[i].Alias)
		}
	}

	if err := RemoveHost("no-such-host"); err == nil {
		t.Error("removing a host that was never registered should be refused")
	}
	if err := RemoveHost("srv-add-test"); err != nil {
		t.Fatalf("RemoveHost: %v", err)
	}
	hosts, err = Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if containsAlias(hosts, "srv-add-test") {
		t.Errorf("removed host still listed: %+v", hosts)
	}
}

func containsAlias(hosts []*config.Host, alias string) bool {
	for _, h := range hosts {
		if h.Alias == alias {
			return true
		}
	}
	return false
}
