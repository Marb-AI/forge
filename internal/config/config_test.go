package config

import "testing"

func TestParseSSHTarget(t *testing.T) {
	cases := []struct {
		in        string
		user      string
		addr      string
		port      int
		expectErr bool
	}{
		{"root@1.2.3.4", "root", "1.2.3.4", 22, false},
		{"deploy@example.com:2222", "deploy", "example.com", 2222, false},
		{"1.2.3.4", "root", "1.2.3.4", 22, false},
		{"host:2200", "root", "host", 2200, false},
		{"user@host:notaport", "", "", 0, true},
		{"", "", "", 0, true},
	}
	for _, c := range cases {
		user, addr, port, err := ParseSSHTarget(c.in)
		if c.expectErr {
			if err == nil {
				t.Errorf("ParseSSHTarget(%q): expected error, got none", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSSHTarget(%q): unexpected error %v", c.in, err)
			continue
		}
		if user != c.user || addr != c.addr || port != c.port {
			t.Errorf("ParseSSHTarget(%q) = (%q,%q,%d), want (%q,%q,%d)",
				c.in, user, addr, port, c.user, c.addr, c.port)
		}
	}
}

func TestRemoveWorkspaceClearsPorts(t *testing.T) {
	c := &Config{
		Hosts:      map[string]*Host{"h": {Alias: "h"}},
		Ports:      map[string]map[string][]int{},
		Workspaces: map[string]string{},
	}
	c.AddWorkspace("crm", "h")
	c.SetPorts("h", "crm", []int{3000})
	c.RemoveWorkspace("crm")
	if _, ok := c.Workspaces["crm"]; ok {
		t.Error("workspace not removed")
	}
	if len(c.Ports) != 0 {
		t.Errorf("ports not cleared: %v", c.Ports)
	}
}
