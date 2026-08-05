package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// callAgent invokes forge-agent on the host over SSH (as the admin user, via
// sudo) and decodes its JSON stdout into out. If the agent reports an error
// (JSON {"error": ...}) that becomes a Go error, regardless of exit status.
//
// Unexported, now that every operation that needs it lives here: the transport is
// this package's own business, and what a front end reaches for is the operation
// — CreateWorkspace, Ports — never the pipe underneath it.
func callAgent(h *config.Host, out any, op string, opArgs ...string) error {
	return callAgentWith(h, nil, out, op, opArgs...)
}

// agentCommand is the argv that runs one agent op on a host.
//
// Split out because three things now build it: the two calls below, and the one
// operation that neither collects JSON nor sends any — chat's tail, which
// streams somebody else's bytes and has no business reassembling this.
func agentCommand(h *config.Host, op string, opArgs ...string) []string {
	// Root needs no sudo (and the box may not even have sudo); a non-root admin
	// uses the passwordless sudoers rule installed by `forge host prepare`.
	head := []string{"forge-agent", op}
	if h.User != "root" {
		head = append([]string{"sudo"}, head...)
	}
	return append(head, opArgs...)
}

// callAgentWith is callAgent with something on the agent's stdin.
//
// One op needs it: a chat prompt is user text of any length, and argv is both
// bounded and readable in ps by every account on the host. nil stdin is the
// ordinary case and means what it says.
func callAgentWith(h *config.Host, in io.Reader, out any, op string, opArgs ...string) error {
	target := sshx.AdminTarget(h)
	remote := agentCommand(h, op, opArgs...)

	var buf bytes.Buffer
	runErr := target.Pipe(in, &buf, os.Stderr, remote...)
	data := buf.Bytes()

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
