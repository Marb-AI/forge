// Package agentproto defines the small JSON vocabulary shared between the CLI
// (laptop) and forge-agent (server). The agent prints one of these as JSON on
// stdout; the CLI decodes it. Keeping the types in one place stops the two
// binaries from drifting apart.
package agentproto

// Status values for a workspace's Claude session.
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
)

// Workspace is the agent's view of a single workspace.
type Workspace struct {
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

// ListResult is returned by `forge-agent workspace-list`.
type ListResult struct {
	Workspaces []Workspace `json:"workspaces"`
}

// CreateResult is returned by `forge-agent workspace-create`.
type CreateResult struct {
	Workspace Workspace `json:"workspace"`
}

// StatusResult is returned by `forge-agent workspace-status`.
type StatusResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// OK is the trivial success payload (e.g. for delete).
type OK struct {
	OK bool `json:"ok"`
}

// ErrorResult is printed (and the process exits non-zero) on failure.
type ErrorResult struct {
	Error string `json:"error"`
}

// TmuxSession is the fixed session name each workspace uses for Claude.
const TmuxSession = "claude"
