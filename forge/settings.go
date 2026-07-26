package forge

import (
	"fmt"

	"github.com/Marb-AI/forge/config"
)

// SetUIPort records the port the browser UI should bind to. It only takes effect
// on the next start — a running daemon already holds the old port.
//
// The range check is the only thing standing between a typo and a daemon that
// won't start, so it lives here rather than in whichever front end asked.
func SetUIPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %d (want 1-65535)", port)
	}
	return updateConfig(func(c *config.Config) error {
		c.UIPort = port
		return nil
	})
}

// UIPort returns the port the browser UI binds to: the configured one, or the
// default when nothing has set it.
//
// The daemon asks for it here rather than reading the config itself. That is the
// whole seam in one line — where the setting is kept is the core's business, and a
// front end that had to know would be a front end that assumes a file.
func UIPort() (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	return cfg.UIPortOr(), nil
}
