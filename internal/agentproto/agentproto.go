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

// PortBlock is the span of host ports one workspace owns — the only host ports it
// is allowed to publish on, and the only ones Forge will tunnel for it.
//
// It is assigned once, at creation, and never moves. That immutability is what the
// whole design rests on: because the block cannot change, the same number means the
// same service forever, so a port can be written into a repo's compose file, an
// OAuth redirect URI or a CORS whitelist and stay correct. It is also why the
// workspace can be *told* its range in plain text (see PortsMemory) instead of
// having to ask at runtime.
//
// Blocks are unique across every host a client knows, not just within one — the
// client allocates them from a single range so that a workspace's remote port can
// also be its local port, with no mapping table and no chance that two servers hand
// out the same number and collide on the laptop that tunnels both.
type PortBlock struct {
	Start int `json:"start"`
	Size  int `json:"size"`
}

// End is the last port in the block. Inclusive: a block at 16000 of size 100 is
// 16000–16099, and the next block starts at 16100. Spelled out here because an
// off-by-one would put two neighbouring workspaces on the same port.
func (b PortBlock) End() int { return b.Start + b.Size - 1 }

// Contains reports whether port is inside the block.
func (b PortBlock) Contains(port int) bool { return port >= b.Start && port <= b.End() }

// Workspace is the agent's view of a single workspace.
type Workspace struct {
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
	// PortBlock is the workspace's port block, or nil for one that has none — a
	// workspace created before this existed. Absent rather than zero because the
	// client allocates blocks by taking the lowest one nobody holds, and a zero
	// block would read as a workspace holding the bottom of the range.
	PortBlock *PortBlock `json:"port_block,omitempty"`
	// Ours is whether this host records the workspace as Forge's.
	//
	// The host's directory holds every account under /home/workspaces, including
	// ones Forge never made — a colleague's, or one made by hand — and until this
	// existed nothing on the machine could tell them apart. Which is why the list
	// of workspaces has had to come from whichever laptop created them, and why a
	// second device sees an empty Forge.
	//
	// Always emitted, never omitted: false is the interesting value here, and a
	// field that vanishes when it is false cannot be told from one an old agent
	// never knew about.
	//
	// Which is not the whole of that problem, and ListResult.Recorded is the rest
	// of it — see there.
	Ours bool `json:"ours"`
}

// ListResult is returned by `forge-agent workspace-list`.
type ListResult struct {
	Workspaces []Workspace `json:"workspaces"`
	// Recorded is whether this host keeps a record of which workspaces are Forge's
	// at all.
	//
	// It is what makes Ours safe to believe. An agent from before the record
	// answers with no `ours` on anything, which unmarshals to false — identical to
	// a host that keeps a record and does not claim these. A client that could not
	// tell those apart would look at an old server, see nothing claimed, and hide
	// every workspace on it.
	//
	// So the question a client asks is this one first: false means "ask your own
	// config, as you always have", and true means the host is the answer.
	Recorded bool `json:"recorded"`
}

// What is holding a port open. A container can be started and stopped, and says
// which internal port it fronts; a plain process — a dev server the workspace ran
// directly — can do neither, and is reported so the tunnel exists and the UI can
// show it rather than pretending it isn't there.
const (
	KindContainer = "container"
	KindProcess   = "process"
)

// Port is one host port a workspace has published, as the agent found it.
//
// This is an OBSERVATION, not a record: nothing is stored, every field is read
// fresh from Docker and the kernel each time. The workspace's block says which
// ports it MAY use; this says which it actually does.
type Port struct {
	// Name is what to call it: the compose service ("web"), or the process name for
	// a plain listener. Never the raw container name, which carries the project
	// prefix and a replica index — "crm-web-1" tells you the workspace you are
	// already looking at, twice.
	Name string `json:"name"`
	Host int    `json:"host"`
	// Target is the port inside the container, which is what the service itself
	// thinks it listens on. Zero for a plain process, where there is no inside.
	Target int `json:"target,omitempty"`
	// Running distinguishes a stopped container from a missing one. A stopped
	// container still HOLDS its host port — `docker start` will bind it — so it must
	// keep appearing, or its port looks free and gets reused.
	Running bool   `json:"running"`
	Kind    string `json:"kind"`
}

// WorkspacePorts is one workspace's block and what it currently publishes.
//
// The block rides along because it is what decides which of these ports get
// tunnelled: exactly the ones inside it. A port outside the block is reported (it
// is real, and worth showing) but never forwarded — the workspace was told in
// writing that its block is what reaches the developer's machine, and quietly
// forwarding more would make that false and let two workspaces collide again.
type WorkspacePorts struct {
	Block *PortBlock `json:"block,omitempty"`
	Ports []Port     `json:"ports"`
}

// PortsResult is returned by `forge-agent workspace-ports`: one entry per workspace
// that has a block or something listening (a workspace with neither has no entry).
type PortsResult struct {
	Workspaces map[string]WorkspacePorts `json:"workspaces"`
}

// Activity states — Claude's attention state within a workspace, as reported by
// the Claude Code hooks the agent installs. The whole vocabulary in one place,
// because the browser UI switches on these strings too.
const (
	ActivityBusy    = "busy"    // Claude is working on your prompt
	ActivityIdle    = "idle"    // Claude finished responding and is waiting for you
	ActivityWaiting = "waiting" // Claude needs your input or a decision
)

// Activity is what a workspace's Claude is up to: its attention state plus the
// unix second the hook that set it fired. The timestamp is what lets the UI tell a
// fresh "waiting for you" from one it has already shown and dismissed.
//
// Topic rides along because it answers the neighbouring question — not "does this
// workspace want me" but "what was I even doing in there", which is the one you
// can't answer yourself once you keep twenty of them. Claude writes it (see
// TopicFile); TopicTS is when, so the UI can say a topic is days old rather than
// present it as current. Both are empty/zero for a workspace that has none.
type Activity struct {
	State   string `json:"state"`
	TS      int64  `json:"ts"`
	Topic   string `json:"topic,omitempty"`
	TopicTS int64  `json:"topic_ts,omitempty"`
}

// ActivityResult is returned by `forge-agent workspace-activity`: one entry per
// workspace that has an activity state or a topic on record (a workspace whose
// Claude has not run since the hooks were installed simply has no entry).
type ActivityResult struct {
	Activity map[string]Activity `json:"activity"`
}

// Track is one workspace's session tracking: SessionStart is the unix second the
// current Claude session began (held across a checkpoint, reset on stop/restart),
// and ActiveSeconds is how long the user has been present at this workspace during
// that session. The agent reads both from ~/.forge-session.json; when that file is
// absent it falls back to the tmux session's own creation time so a plain running
// session still reports a start.
type Track struct {
	SessionStart  int64 `json:"session_start"`
	ActiveSeconds int64 `json:"active_seconds"`
}

// TrackResult is returned by `forge-agent workspace-track`: one entry per running
// workspace (a stopped session — whose marker file was removed — has no entry).
type TrackResult struct {
	Sessions map[string]Track `json:"sessions"`
}

// RateWindow is one of a Claude subscription's rate-limit windows — how much of
// it is spent and when it starts over. Both numbers come from Claude Code itself
// (the statusLine payload), which is the only place they exist: nothing on the
// disk records them, they arrive with an API response.
//
// A window is reported as a pointer everywhere it appears, because absent and
// zero are different answers. Absent means nobody has asked the API yet under
// this login; 0% means the window is genuinely untouched. Showing an empty bar
// for the first would be a confident lie.
type RateWindow struct {
	UsedPercent float64 `json:"used_percent"`
	// ResetsAt is the unix second the window rolls over, or 0 if Claude did not
	// say.
	ResetsAt int64 `json:"resets_at,omitempty"`
}

// Account identifies the Claude login a workspace is signed in as, read from that
// workspace's own ~/.claude.json. Workspaces on one server can be signed in as
// different accounts — each is a separate Linux user with its own credentials —
// and the account is what the rate-limit windows above are actually measured
// against, so it is the key the UI groups by.
//
// UUID is that key, not Email: the address is the label a human reads, but the
// same person signed into two organisations is two accounts with two sets of
// limits. Everything but UUID may be empty — a login that has not fetched its
// profile yet has the id and little else.
type Account struct {
	UUID  string `json:"uuid"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Org   string `json:"org,omitempty"`
}

// How a workspace's Claude pays for itself. Not every deployment is a Claude.ai
// subscription, and the difference decides which of the numbers below mean
// anything: the 5-hour and weekly windows exist only for Pro/Max, while an
// organisation on API credits has no such windows at all — for them the honest
// headline is spend, not a percentage of an allowance that isn't there.
//
// It is detected, never configured, and reported so the UI can group and label
// truthfully instead of showing a workspace with no rate limits as one whose limits
// are unknown. AuthUnknown is the honest answer when nothing on the host says.
const (
	AuthSubscription = "subscription" // signed in to a Claude.ai account (Pro/Max)
	AuthAPIKey       = "api"          // an Anthropic API key, i.e. credits
	AuthBedrock      = "bedrock"      // Amazon Bedrock
	AuthVertex       = "vertex"       // Google Vertex AI
	AuthUnknown      = ""             // nothing on the host says
)

// Usage is one workspace's Claude usage: which login it runs as, how full its
// context window is, what the session has cost, and where that login stands
// against its 5-hour and weekly limits.
//
// The two halves have different lifetimes, which is why an entry can hold either
// alone. Account comes from ~/.claude.json and survives everything — a stopped
// workspace still reports the login it will come back as, which is the whole
// point of showing it. The rest is a sample the statusLine command left behind
// the last time Claude rendered, so it is only as current as TS says.
//
// The rate-limit windows are per account, not per workspace: three workspaces on
// one login all report the same 5-hour figure, because they are all spending the
// same allowance. They ride along on each workspace because that is where the
// sample is taken, and the UI groups them back together.
type Usage struct {
	Account Account `json:"account"`
	// Auth is how this workspace pays — one of the Auth* constants above. It is what
	// tells "this login has 41% of its week left" apart from "this workspace has no
	// weekly window because it bills credits", which are the same absent fields.
	Auth string `json:"auth,omitempty"`
	// TS is the unix second the sample was written. Zero means there is no sample
	// — the statusLine command has not run in this workspace yet — and every
	// field below it is meaningless rather than zero.
	TS int64 `json:"ts,omitempty"`
	// Model is Claude's display name for the model the session is on ("Opus 4.6").
	Model string `json:"model,omitempty"`
	// ContextUsed is the tokens currently in the context window (fresh input plus
	// cache reads and writes, which is how Claude Code itself counts it);
	// ContextSize is the window they are measured against. A zero ContextSize is
	// "not reported" — no session has a zero-token window — so the UI can tell an
	// unmeasured context from an empty one, the same way HostStats does.
	ContextUsed int64 `json:"context_used,omitempty"`
	ContextSize int64 `json:"context_size,omitempty"`
	// CostUSD is what this session has spent so far, as Claude Code accounts it.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// FiveHour and SevenDay are the login's rate-limit windows, absent unless the
	// account is a Claude.ai subscription that has made at least one API call this
	// session.
	FiveHour *RateWindow `json:"five_hour,omitempty"`
	SevenDay *RateWindow `json:"seven_day,omitempty"`
	// Note is the few words explaining why this workspace's figures are missing or
	// frozen when the reason is knowable and is not "Claude hasn't run" — today, a
	// higher-precedence Claude Code setting owning the status line we collect
	// through. Empty when there is nothing to explain. Same idea as HostStats' note:
	// a panel saying why it has no number beats one that just doesn't.
	Note string `json:"note,omitempty"`
}

// UsageResult is returned by `forge-agent workspace-usage`: one entry per
// workspace that has a login on record or a usage sample (a workspace nobody has
// signed into has neither, and simply has no entry) — the same shape as
// ActivityResult.
type UsageResult struct {
	Usage map[string]Usage `json:"usage"`
}

// HostStats is one server's resource usage at the moment it was asked, and the
// whole payload of `forge-agent host-stats` (no wrapper: a host speaks for
// itself). Byte counts rather than percentages, because "83% full" and "12 GB
// left" are different questions and only the raw numbers answer both.
//
// CPUPercent is the busy share of all cores over a short sampling window inside
// the agent run — /proc/stat is a set of counters since boot, so a single read
// would report the average since the machine came up, which is not what "cpu
// usage" means to anyone looking at it.
type HostStats struct {
	CPUPercent float64 `json:"cpu_percent"`
	CPUCores   int     `json:"cpu_cores"`
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	// DiskPath is the filesystem measured — where workspaces live, falling back to
	// "/" — reported so the UI can say which disk it is talking about.
	DiskPath  string `json:"disk_path"`
	DiskTotal uint64 `json:"disk_total"`
	DiskUsed  uint64 `json:"disk_used"`
	// Uptime is how long the machine has been up, in seconds.
	Uptime int64 `json:"uptime"`
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

// VersionResult is what `forge-agent version` answers: which build of Forge
// installed this agent. The client carries the agent it uploads, so this is the
// version of the client that last prepared the server — the answer to "is the
// far end older than the thing asking".
type VersionResult struct {
	Version string `json:"version"`
}

// ChatTurn is what starting a turn answers with: the id to read it back by.
type ChatTurn struct {
	Turn string `json:"turn"`
}

// PortRangeResult is a host's answer about which of its ports Forge may hand
// out.
//
// Set and Recorded are two different absences and both matter. Recorded false is
// an agent that predates this — ask your own config, as always. Recorded true
// with Set false is a host that keeps state and has never been told its range,
// which is every host between the record arriving and a client filling it in.
type PortRangeResult struct {
	Start    int  `json:"start"`
	End      int  `json:"end"`
	Block    int  `json:"block"`
	Set      bool `json:"set"`
	Recorded bool `json:"recorded"`
}

// AuthorizeResult is which workspaces a newly authorised key was let into. The
// host login is not listed: it is not optional, and a failure there is an error
// rather than a shorter list.
type AuthorizeResult struct {
	Workspaces []string `json:"workspaces"`
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

// SessionFile is the per-workspace session-tracking file, in the workspace home.
// It holds {session_start, active_seconds}; its presence means a session is being
// tracked. The agent reads it (workspace-track); the workspace user's own commands
// below create and clear it. A hidden dotfile so it stays out of the file tree.
const SessionFile = ".forge-session.json"

// TopicFile is the per-workspace topic file, relative to the workspace home. It
// holds "<unix-seconds> <one line of text>" — written by the `forge-topic` command
// Claude runs, read by the agent on its activity sweep. It sits in ~/.claude
// beside forge-activity because it is Claude's own scribble about its own session,
// and because that directory is already hidden from the file tree.
const TopicFile = ".claude/forge-topic"

// UsageFile is the per-workspace usage sample, relative to the workspace home. It
// holds one JSON object — see the agent's usageCmdScript, which writes it — read
// by the agent on its usage sweep. It sits in ~/.claude beside forge-activity and
// forge-topic for the same reasons: it is Claude's own account of its own session,
// and that directory is already hidden from the file tree.
const UsageFile = ".claude/forge-usage"

// ChatDir holds the chat transcripts, relative to the workspace home. One
// directory beside forge-activity, forge-topic and forge-usage, for the same two
// reasons: it is Claude's own account of its own session, and ~/.claude is
// already hidden from the file tree.
//
// A turn is four files sharing a name — the prompt that started it, the stream
// it produced, whatever it said on stderr, and a marker written when the process
// exited. Files rather than a database because of what has to be true when a phone comes back after twenty
// minutes on a train: the turn ran to the end without anyone watching, and what
// it said is still there to be read from wherever the reader got to. An offset
// into a file is the whole of that reader's state.
const ChatDir = ".claude/forge-chat"

// The four files of one turn, and the suffixes that name them. Sharing a stem
// means a turn is one glob, which is what makes "clean up after turn X" and
// "list the turns" the same shape of thing.
const (
	ChatPromptSuffix = ".prompt" // what was asked, as sent to claude -p on stdin
	ChatStreamSuffix = ".jsonl"  // stream-json, exactly as it came out
	ChatDoneSuffix   = ".done"   // the exit status, written when the turn ended
	ChatErrSuffix    = ".err"    // stderr, kept apart so it cannot corrupt the stream
)

// ChatTurnEvent is the type of the one line in a chat stream that Claude Code
// did not write: Forge's own, introducing a turn in a replayed conversation with
// its id and the prompt that started it.
//
// The prompt is not in Claude Code's output at all — it went in on stdin — so a
// transcript without this is a page of answers to questions nobody can see. The
// name is Forge's on purpose: a reader that does not know it skips it, which is
// exactly what a reader of a live turn should do.
const ChatTurnEvent = "forge_turn"

// ChatSessionFile records the Claude session the next turn resumes, relative to
// the workspace home. One line, the id from the last turn's result.
//
// This is what makes a chat a conversation rather than a series of strangers:
// Claude Code keeps the history itself, on this host, and --resume takes it. The
// transcripts beside it are for the reader, not for Claude.
const ChatSessionFile = ChatDir + "/session"

// ChatTmuxSession is the tmux session a chat turn runs in — one per workspace,
// not one per turn, so "is a turn already running" is a question tmux can answer
// and two prompts cannot race into the same conversation.
//
// It is tmux for one reason, and it is not the terminal: a turn must survive the
// phone that started it going into a tunnel. Nothing reads this session's screen.
const ChatTmuxSession = "forge-chat"

// AccountFile is where Claude Code keeps the signed-in account, relative to the
// workspace home. Its `oauthAccount` object is the only record of which login a
// workspace uses, and unlike the usage sample it is there whether or not Claude is
// running.
const AccountFile = ".claude.json"

// TopicMaxRunes is how much of a topic survives to the UI. A topic is a label, not
// a summary: it goes in a narrow sidebar and a tab tooltip, so anything longer is
// cut. Enforced on the way out (rune-safe) as well as on the way in, because the
// file is plain text that anything could have written.
const TopicMaxRunes = 120

// LabelMaxRunes bounds the short labels that come out of a workspace's own Claude
// state — the model name, and the login's email, display name and organisation.
// Shorter than a topic because these are identifiers, not sentences: they go in a
// group header in a narrow column, and anything longer is something odd in a file
// nobody promised us the shape of.
const LabelMaxRunes = 80

// ClearSession removes the tracking file, run as the workspace user. Appended to
// the stop and restart commands so a stopped or hard-restarted session starts its
// clocks over — unlike a checkpoint, which deliberately keeps them.
const ClearSession = `rm -f "$HOME/` + SessionFile + `" 2>/dev/null || true`

// FreezeSession pins the current session's start into the tracking file if it is
// not there yet, run as the workspace user just before a checkpoint kills the tmux
// session. Without it, the fresh session created by the checkpoint would report its
// own (later) creation time and the session clock would jump. Create-if-absent: a
// file already tracking activity keeps its earlier start untouched.
const FreezeSession = `sc=$(tmux display -p -t ` + TmuxSession + ` '#{session_created}' 2>/dev/null); ` +
	`f="$HOME/` + SessionFile + `"; ` +
	`{ [ -n "$sc" ] && [ ! -f "$f" ] && printf '{"session_start":%s,"active_seconds":0}\n' "$sc" > "$f"; } || true`

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

	named := "claude -n " + ShellQuote(name) + " " + ShellQuote(prompt)
	plain := "claude " + ShellQuote(prompt)
	// Asking Claude what it supports beats guessing from a version string, and it
	// costs one local --help on a path that already takes minutes.
	inner := "if claude --help 2>/dev/null | grep -q -- --name; then " + named + "; else " + plain + "; fi"

	return SourceEnv + "tmux new -d -s " + TmuxSession + " " + ShellQuote(inner)
}

// ShellQuote wraps s so a POSIX shell reads it back as one literal argument.
// These commands are assembled here and run verbatim on the server, through two
// shells (ssh's, then the one tmux starts), so each layer has to be quoted on the
// way in — an em dash or an apostrophe in a prompt must not become syntax.
func ShellQuote(s string) string {
	// A single quote can't appear inside single quotes, so each one closes the
	// string, contributes an escaped quote, and opens it again: ' -> '\''
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
