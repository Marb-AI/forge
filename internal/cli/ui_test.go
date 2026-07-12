package cli

import (
	"testing"

	"github.com/Marb-AI/forge/internal/config"
)

// setUIPort is what both `forge ui port` and the UI's settings panel go through,
// so its range check is the only thing standing between a typo and a daemon that
// won't start.
func TestSetUIPortRejectsOutOfRange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, p := range []int{0, -1, 65536, 1 << 20} {
		if err := setUIPort(p); err == nil {
			t.Errorf("setUIPort(%d) should be refused", p)
		}
	}

	// A refused port must not have been written, or the next start would try to
	// bind it anyway.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPort != 0 {
		t.Errorf("a rejected port was persisted anyway: %d", cfg.UIPort)
	}
}

func TestSetUIPortPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, p := range []int{1, 8099, 65535} {
		if err := setUIPort(p); err != nil {
			t.Fatalf("setUIPort(%d): %v", p, err)
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UIPort != p || cfg.UIPortOr() != p {
			t.Errorf("port %d not persisted (got %d)", p, cfg.UIPort)
		}
	}
}
