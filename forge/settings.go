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
	return config.Update(func(c *config.Config) error {
		c.UIPort = port
		return nil
	})
}
