package cli

import (
	"encoding/json"
	"fmt"

	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/config"
	"github.com/Marb-AI/forge/internal/sshx"
)

// callAgent invokes forge-agent on the host over SSH (as the admin user, via
// sudo) and decodes its JSON stdout into out. If the agent reports an error
// (JSON {"error": ...}) that becomes a Go error, regardless of exit status.
func callAgent(h *config.Host, out any, op string, opArgs ...string) error {
	target := sshx.AdminTarget(h)
	remote := append([]string{"sudo", "forge-agent", op}, opArgs...)
	data, runErr := sshx.Capture(target.Args(remote...)...)

	// The agent prints a JSON error even when it exits non-zero; prefer it.
	var maybeErr agentproto.ErrorResult
	if len(data) > 0 && json.Unmarshal(data, &maybeErr) == nil && maybeErr.Error != "" {
		return fmt.Errorf("agent: %s", maybeErr.Error)
	}
	if runErr != nil {
		return fmt.Errorf("ssh/forge-agent failed: %w", runErr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode agent response: %w", err)
	}
	return nil
}
