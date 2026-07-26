package forge

import "testing"

// SetUIPort is what both `forge ui port` and the UI's settings panel go through,
// so its range check is the only thing standing between a typo and a daemon that
// won't start.
//
// It writes a real config file, under the throwaway HOME this package's TestMain
// installs — never the developer's.
func TestSetUIPortRejectsOutOfRange(t *testing.T) {
	before, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []int{0, -1, 65536, 1 << 20} {
		if err := SetUIPort(p); err == nil {
			t.Errorf("SetUIPort(%d) should be refused", p)
		}
	}

	// A refused port must not have been written, or the next start would try to
	// bind it anyway.
	after, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if after.UIPort != before.UIPort {
		t.Errorf("a rejected port was persisted anyway: %d (was %d)", after.UIPort, before.UIPort)
	}
}

func TestSetUIPortPersists(t *testing.T) {
	for _, p := range []int{1, 8099, 65535} {
		if err := SetUIPort(p); err != nil {
			t.Fatalf("SetUIPort(%d): %v", p, err)
		}
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UIPort != p || cfg.UIPortOr() != p {
			t.Errorf("port %d not persisted (got %d)", p, cfg.UIPort)
		}
	}
}
