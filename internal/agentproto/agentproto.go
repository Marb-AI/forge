// Package agentproto defines the small JSON vocabulary shared between the CLI
// (laptop) and forge-agent (server). The agent prints one of these as JSON on
// stdout; the CLI decodes it. Keeping the types in one place stops the two
// binaries from drifting apart.
package agentproto

import "strings"

// Status values for a workspace's Claude session — the whole vocabulary, in one
// place, because the browser UI switches on these strings too and a rename that
// only happened here would silently mislabel every workspace.
//
// The agent emits the first two: it can only speak for workspaces the host has.
// The client adds the last two, which describe the gap between what its config
// claims and what the host really has.
const (
	StatusRunning = "running"
	StatusStopped = "stopped"

	// StatusMissing: our config records the workspace; the host says it doesn't
	// have it. Deleted from another machine, most likely. Reporting it as "stopped"
	// would be a lie you could act on — there is nothing left to start.
	StatusMissing = "missing"
	// StatusUnreachable: we could not ask the host, so we do not know.
	StatusUnreachable = "unreachable"
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

// Activity states — Claude's attention state within a workspace, as reported by
// the Claude Code hooks the agent installs. The whole vocabulary in one place,
// because the browser UI switches on these strings too.
const (
	ActivityBusy    = "busy"    // Claude is working on your prompt
	ActivityIdle    = "idle"    // Claude finished responding and is waiting for you
	ActivityWaiting = "waiting" // Claude needs your input or a decision
)

// Activity is one workspace's attention state plus the unix second the hook that
// set it fired. The timestamp is what lets the UI tell a fresh "waiting for you"
// from one it has already shown and dismissed.
type Activity struct {
	State string `json:"state"`
	TS    int64  `json:"ts"`
}

// ActivityResult is returned by `forge-agent workspace-activity`: one entry per
// workspace that has an activity state on record (workspaces whose Claude has not
// run since the hooks were installed simply have no entry).
type ActivityResult struct {
	Activity map[string]Activity `json:"activity"`
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

// SourceEnv is the prelude every workspace command runs: it sources the
// workspace env file so the session inherits COMPOSE_PROJECT_NAME et al. even
// though it isn't an interactive login shell. `set -a` exports what it sources.
const SourceEnv = `set -a; [ -f "$HOME/.forge/env" ] && . "$HOME/.forge/env"; set +a; `

// The remote commands that drive a workspace's Claude session. Both front ends —
// the CLI and the browser UI — build them here rather than each spelling out its
// own tmux invocation, so the two can't drift apart.
const (
	// AttachClaude attaches the session, creating it if it isn't there. This is
	// what a terminal (or the browser) runs to get a live session.
	AttachClaude = SourceEnv + "tmux new -A -s " + TmuxSession + " claude"

	// StartClaude starts a fresh session detached — used by a hard restart, where
	// nobody is attached yet.
	StartClaude = SourceEnv + "tmux new -d -s " + TmuxSession + " claude"

	// KillClaude ends the session if it exists and succeeds either way, so only a
	// connection failure surfaces as an error.
	KillClaude = "tmux kill-session -t " + TmuxSession + " 2>/dev/null || true"
)

// ResumeClaude starts a fresh session detached and tells Claude to pick up from
// the handoff it just wrote. This is the tail of a checkpoint.
//
// The session is given an explicit name, because a checkpoint used to leave
// nothing to tell one resumable chat from another. Every checkpoint launched
// Claude with the identical first message, and an unnamed session takes its title
// from a summary of that message — so the resume picker filled up with rows that
// all read "Continue from memory", in the one place you need to tell them apart.
// The name carries the workspace and the moment: "marbai-01 2026-07-20 14:03".
//
// -n is recent, and these commands run on servers provisioned whenever the user
// happened to provision them. An unknown flag would make Claude exit at once,
// leaving the checkpoint with a killed session and nothing in its place — the
// handoff is safe in memory, but the workspace looks dead. So the flag is used
// only where it exists, and the fallback still gets a distinguishable title the
// old way: by leading the prompt with the same words the name would have used.
func ResumeClaude(workspace, stamp string) string {
	name := workspace + " " + stamp
	prompt := name + " — continue from memory: read the handoff you just wrote and carry on from it."

	named := "claude -n " + shellQuote(name) + " " + shellQuote(prompt)
	plain := "claude " + shellQuote(prompt)
	// Asking Claude what it supports beats guessing from a version string, and it
	// costs one local --help on a path that already takes minutes.
	inner := "if claude --help 2>/dev/null | grep -q -- --name; then " + named + "; else " + plain + "; fi"

	return SourceEnv + "tmux new -d -s " + TmuxSession + " " + shellQuote(inner)
}

// shellQuote wraps s so a POSIX shell reads it back as one literal argument.
// These commands are assembled here and run verbatim on the server, through two
// shells (ssh's, then the one tmux starts), so each layer has to be quoted on the
// way in — an em dash or an apostrophe in a prompt must not become syntax.
func shellQuote(s string) string {
	// A single quote can't appear inside single quotes, so each one closes the
	// string, contributes an escaped quote, and opens it again: ' -> '\''
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
