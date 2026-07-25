// Package agent implements forge-agent: the small privileged helper that runs
// on the server, invoked over SSH per operation (never a long-lived daemon).
// It owns only what needs root — workspace lifecycle via ordinary Linux tools
// (useradd, tmux, the filesystem). Everything it prints is JSON.
//
// It is Linux-only by design (it manages Linux users); it will build on any
// platform but is meant to run on the VPS.
package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// baseDir is where workspace home directories live. A variable, not a constant, so
// tests can point it at a fixture (the same seam as procRoot).
var baseDir = "/home/workspaces"

// metadataFile is the per-workspace metadata file (name, owner, created_at). A
// hidden dotfile so it stays out of the browser's file tree — nothing reads it at
// runtime; it is written once at creation. Older workspaces have the pre-rename
// visible name and are moved in place on the next sweep (see migrateMetadata).
const metadataFile = ".workspace.json"

// nameRe restricts workspace names to safe Linux usernames — these become paths
// and command arguments, so we validate strictly.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Main is the forge-agent entrypoint; returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		return emitError("usage: forge-agent <workspace-create|workspace-delete|workspace-list|workspace-status|workspace-activity|workspace-track|workspace-track-inc|workspace-usage|host-stats>")
	}
	switch args[0] {
	case "workspace-create":
		return opCreate(args[1:])
	case "workspace-delete":
		return opDelete(args[1:])
	case "workspace-list":
		return opList()
	case "workspace-status":
		return opStatus(args[1:])
	case "workspace-activity":
		return opActivity()
	case "workspace-track":
		return opTrack()
	case "workspace-track-inc":
		return opTrackInc(args[1:])
	case "workspace-usage":
		return opUsage()
	case "host-stats":
		return opHostStats()
	default:
		return emitError("unknown op %q", args[0])
	}
}

func opCreate(args []string) int {
	fs := flag.NewFlagSet("workspace-create", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	pubkeyB64 := fs.String("pubkey", "", "base64-encoded SSH public key")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	pubkey, err := base64.StdEncoding.DecodeString(*pubkeyB64)
	if err != nil || len(pubkey) == 0 {
		return emitError("invalid --pubkey")
	}

	home := filepath.Join(baseDir, *name)
	if _, err := os.Stat(home); err == nil {
		return emitError("workspace %q already exists", *name)
	}

	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return emitError("mkdir %s: %v", baseDir, err)
	}
	// Create the Linux user with its home under /home/workspaces.
	if out, err := run("useradd", "-m", "-d", home, "-s", "/bin/bash", *name); err != nil {
		return emitError("useradd: %v: %s", err, out)
	}
	// Best-effort: let the workspace use docker (soft isolation, by design).
	if out, err := run("usermod", "-aG", "docker", *name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not add %s to docker group: %v: %s\n", *name, err, out)
	}

	if err := seedSSH(home, *name, pubkey); err != nil {
		return emitError("ssh setup: %v", err)
	}
	if err := seedGitKey(home, hostKeyDir); err != nil {
		return emitError("git key: %v", err)
	}
	if err := seedGhAuth(home, hostGhDir); err != nil {
		return emitError("gh auth: %v", err)
	}
	if err := writeEnvFile(home, *name); err != nil {
		return emitError("env file: %v", err)
	}
	if err := seedBashrc(home, *name); err != nil {
		return emitError("bashrc: %v", err)
	}
	if err := seedGitconfig(home); err != nil {
		return emitError("gitconfig: %v", err)
	}
	if err := seedTmuxConf(home); err != nil {
		return emitError("tmux conf: %v", err)
	}
	if err := writeMetadata(home, *name); err != nil {
		return emitError("metadata: %v", err)
	}
	// Own everything by the workspace user.
	if out, err := run("chown", "-R", *name+":"+*name, home); err != nil {
		return emitError("chown: %v: %s", err, out)
	}

	// Install Claude Code as the workspace user — a workspace exists to run it.
	// The native installer drops the binary in ~/.local/bin (on PATH via the env
	// file). Authentication is not handled here: the first `claude` run inside the
	// tmux session surfaces the login prompt interactively.
	if out, err := run("runuser", "-l", *name, "-c", "curl -fsSL https://claude.ai/install.sh | bash"); err != nil {
		return emitError("claude install: %v: %s", err, tailLines(out, 6))
	}

	// Pre-configure Claude so a session starts cleanly with nobody at the keyboard:
	// pre-trust the workspace folder and skip permission prompts.
	if err := seedClaudeConfig(home, *name); err != nil {
		return emitError("claude config: %v", err)
	}

	// The topic and usage commands go in after the Claude install, which is what
	// creates ~/.local/bin in the first place.
	if err := seedTopicCmd(home, *name); err != nil {
		return emitError("topic cmd: %v", err)
	}
	if err := seedUsageCmd(home, *name); err != nil {
		return emitError("usage cmd: %v", err)
	}

	return emit(agentproto.CreateResult{Workspace: agentproto.Workspace{
		Name: *name, Owner: *name, Status: agentproto.StatusStopped,
	}})
}

func opDelete(args []string) int {
	fs := flag.NewFlagSet("workspace-delete", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	// Kill any running session first (ignore failure — may not exist).
	_, _ = run("runuser", "-l", *name, "-c", "tmux kill-server")
	// Then make sure *nothing* of the user is left running, or userdel refuses
	// ("user X is currently used by process N", exit 8) and the delete fails.
	if err := reapUser(*name); err != nil {
		return emitError("%v", err)
	}
	if out, err := run("userdel", "-r", *name); err != nil {
		return emitError("userdel: %v: %s", err, out)
	}
	return emit(agentproto.OK{OK: true})
}

// procRoot is the procfs mount; a variable so tests can point it at a fixture.
var procRoot = "/proc"

// reapGrace is how long we give the user's processes to exit after a signal —
// once after SIGTERM (so a shell or Claude can wind down), once after SIGKILL.
// reapPoll is how often we re-read the process table while waiting.
const (
	reapGrace = 5 * time.Second
	reapPoll  = 100 * time.Millisecond
)

// reapUser ends every process owned by the workspace user, and returns only once
// none are left. `userdel` refuses to remove a user that still owns a process, so
// delete must clear them out first — and killing the tmux server alone does not:
// the sshd sessions behind the browser's terminals and file pane die on their own
// schedule, milliseconds after we drop the connection, which is exactly the window
// userdel looks in. Waiting for the process table, rather than for a timer, is
// what makes the delete deterministic instead of a coin flip.
//
// SIGTERM first, SIGKILL for whatever ignores it. A user who does not exist is
// not an error — there is nothing to reap, and userdel will say so better than we
// can. A lookup that fails for any *other* reason is: swallowing it would skip the
// reaping silently and hand the delete straight back to the bug this exists to fix.
func reapUser(name string) error {
	u, err := user.Lookup(name)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up %s: %v", name, err)
	}
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		pids, err := userPIDs(procRoot, u.Uid)
		if err != nil {
			return fmt.Errorf("scan processes of %s: %v", name, err)
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			p, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			// A process that exited between the scan and here is the outcome we
			// wanted anyway, so a failed signal is not worth reporting.
			_ = p.Signal(sig)
		}
		if waitReaped(u.Uid, reapGrace) {
			return nil
		}
	}
	pids, _ := userPIDs(procRoot, u.Uid)
	return fmt.Errorf("processes of %s would not die: %v", name, pids)
}

// waitReaped polls until the user owns no processes, or the deadline passes.
func waitReaped(uid string, within time.Duration) bool {
	for waited := time.Duration(0); ; waited += reapPoll {
		pids, err := userPIDs(procRoot, uid)
		if err == nil && len(pids) == 0 {
			return true
		}
		if waited >= within {
			return false
		}
		time.Sleep(reapPoll)
	}
}

// userPIDs lists the processes whose real UID is uid, by reading procfs — no
// pgrep, so the agent depends on nothing the server might not have installed.
// Our own PID is skipped: the agent runs as root, so it should never match, but
// signalling ourselves mid-delete is not a mistake worth leaving possible.
func userPIDs(root, uid string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pids []int
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue // not a process directory (or it is us)
		}
		owner, err := procUID(filepath.Join(root, e.Name(), "status"))
		if err != nil {
			continue // it exited while we looked: nothing to kill
		}
		if owner == uid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// procUID reads the real UID from a /proc/<pid>/status file, whose Uid line is
// "Uid:\treal\teffective\tsaved\tfs".
func procUID(statusPath string) (string, error) {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		if f := strings.Fields(rest); len(f) > 0 {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("no Uid line in %s", statusPath)
}

func opList() int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return emit(agentproto.ListResult{Workspaces: []agentproto.Workspace{}})
		}
		return emitError("read %s: %v", baseDir, err)
	}
	list := agentproto.ListResult{Workspaces: []agentproto.Workspace{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		list.Workspaces = append(list.Workspaces, agentproto.Workspace{
			Name: name, Owner: name, Status: sessionStatus(name),
		})
	}
	return emit(list)
}

func opStatus(args []string) int {
	fs := flag.NewFlagSet("workspace-status", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	return emit(agentproto.StatusResult{Name: *name, Status: sessionStatus(*name)})
}

// sessionStatus reports whether the workspace's Claude tmux session is running,
// checked as the workspace user (each user has its own tmux server).
func sessionStatus(name string) string {
	cmd := fmt.Sprintf("tmux has-session -t %s", agentproto.TmuxSession)
	if _, err := run("runuser", "-l", name, "-c", cmd); err != nil {
		return agentproto.StatusStopped
	}
	return agentproto.StatusRunning
}

// opActivity reports each workspace's Claude attention state (busy/idle/waiting),
// which the Claude Code hooks record in ~/.claude/forge-activity, and its current
// topic, which Claude itself writes with `forge-topic`. It also lazily installs
// both — the hooks and the command — into any workspace still missing them, so a
// workspace made before this existed starts reporting on its own, no re-provision
// needed. A workspace with neither on record simply has no entry.
func opActivity() int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return emit(agentproto.ActivityResult{Activity: map[string]agentproto.Activity{}})
		}
		return emitError("read %s: %v", baseDir, err)
	}
	res := agentproto.ActivityResult{Activity: map[string]agentproto.Activity{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		ensureActivityHooks(name)
		ensureTopicCmd(name)
		migrateMetadata(name) // piggy-back the metadata-file hide on the frequent sweep
		a, haveActivity := readActivity(name)
		// The topic outlives the attention state it was written under: a stopped
		// workspace still reports what it was doing, which is the whole point of
		// having one. So an entry is worth emitting if either half is there.
		topic, ts, haveTopic := readTopic(name)
		a.Topic, a.TopicTS = topic, ts
		if haveActivity || haveTopic {
			res.Activity[name] = a
		}
	}
	return emit(res)
}

// readActivity reads ~/.claude/forge-activity for a workspace. Absent (Claude
// hasn't run since the hooks landed) → not ok.
func readActivity(name string) (agentproto.Activity, bool) {
	data, err := os.ReadFile(filepath.Join(baseDir, name, ".claude", "forge-activity"))
	if err != nil {
		return agentproto.Activity{}, false
	}
	return parseActivity(data)
}

// parseActivity reads the hooks' "<state> <unix-seconds>" line. Empty → not ok.
func parseActivity(data []byte) (agentproto.Activity, bool) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return agentproto.Activity{}, false
	}
	a := agentproto.Activity{State: fields[0]}
	if len(fields) > 1 {
		a.TS, _ = strconv.ParseInt(fields[1], 10, 64)
	}
	return a, true
}

// readTopic reads ~/.claude/forge-topic for a workspace. Absent (nobody has set
// one) → not ok.
func readTopic(name string) (string, int64, bool) {
	data, err := os.ReadFile(filepath.Join(baseDir, name, agentproto.TopicFile))
	if err != nil {
		return "", 0, false
	}
	return parseTopic(data)
}

// parseTopic reads the "<unix-seconds> <text>" line `forge-topic` writes.
//
// The command already sanitises what it writes, but this parses the file rather
// than trusting it: it is plain text in a home directory, and the model that fills
// it in can put anything in the argument. So the text is flattened to one line,
// stripped of control characters (which would otherwise travel through JSON into
// the browser's DOM) and cut to length here as well — rune-safe, unlike a byte cut
// in the shell, so a truncated topic can't end in half a character.
//
// A line with no text, or a timestamp that isn't one, is not a topic.
func parseTopic(data []byte) (string, int64, bool) {
	stamp, rest, found := strings.Cut(strings.TrimSpace(string(data)), " ")
	if !found {
		return "", 0, false
	}
	ts, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", 0, false
	}
	text := sanitizeText(rest, agentproto.TopicMaxRunes)
	if text == "" {
		return "", 0, false
	}
	return text, ts, true
}

// sanitizeText makes text that came out of a workspace home safe to report: one
// line, no control characters (which would otherwise travel through JSON into the
// browser's DOM), cut to a rune bound — rune-safe, unlike a byte cut, so a
// truncated string can't end in half a character — with an ellipsis marking the
// cut.
//
// Everything the agent reads from a file a workspace user can write goes through
// here: the topic, the model name, the account labels. None of it is trusted input,
// whatever wrote it last.
func sanitizeText(s string, max int) string {
	text := strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}), " ")
	if runes := []rune(text); len(runes) > max {
		text = strings.TrimRight(string(runes[:max]), " ") + "…"
	}
	return text
}

// opTrack reports each running workspace's session tracking: when the current
// Claude session began and how long the user has been present at it. The numbers
// live in ~/.forge-session.json (written by opTrackInc and the workspace's own
// freeze/clear commands); a workspace whose session is stopped — its file removed —
// simply has no entry, the same shape as opActivity.
func opTrack() int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return emit(agentproto.TrackResult{Sessions: map[string]agentproto.Track{}})
		}
		return emitError("read %s: %v", baseDir, err)
	}
	res := agentproto.TrackResult{Sessions: map[string]agentproto.Track{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if t, ok := readTrack(e.Name()); ok {
			res.Sessions[e.Name()] = t
		}
	}
	return emit(res)
}

// readTrack returns a workspace's session tracking. The file is authoritative when
// present. When it is absent we fall back to the tmux session's creation time, so a
// plain running session (no activity flushed yet, no checkpoint yet) still reports a
// start; with no file and no session there is nothing to track and ok is false.
func readTrack(name string) (agentproto.Track, bool) {
	var t agentproto.Track
	if data, err := os.ReadFile(filepath.Join(baseDir, name, agentproto.SessionFile)); err == nil {
		_ = json.Unmarshal(data, &t) // tolerate garbage: fall through to the tmux start
	}
	if t.SessionStart == 0 {
		sc := sessionCreated(name)
		if sc == 0 {
			return agentproto.Track{}, false
		}
		t.SessionStart = sc
	}
	return t, true
}

// sessionCreated returns the unix second the workspace's Claude tmux session was
// created, or 0 if there is none (run as the workspace user, whose tmux server it
// is — the same runuser dance as sessionStatus).
func sessionCreated(name string) int64 {
	out, err := run("runuser", "-l", name, "-c",
		fmt.Sprintf("tmux display -p -t %s '#{session_created}'", agentproto.TmuxSession))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}

// opTrackInc adds seconds of user-present time to a workspace's session tracking,
// creating the file — and pinning session_start to the current session's creation
// time — on first write. The browser flushes its accumulated activity here every so
// often (and on leaving), so the count survives a reload or a dropped connection.
func opTrackInc(args []string) int {
	fs := flag.NewFlagSet("workspace-track-inc", flag.ContinueOnError)
	name := fs.String("name", "", "workspace name")
	seconds := fs.Int("seconds", 0, "seconds of activity to add")
	if err := fs.Parse(args); err != nil {
		return emitError("bad arguments")
	}
	if !nameRe.MatchString(*name) {
		return emitError("invalid workspace name %q", *name)
	}
	if *seconds <= 0 {
		return emit(agentproto.OK{OK: true}) // nothing to add; not an error
	}
	path := filepath.Join(baseDir, *name, agentproto.SessionFile)
	err := mergeJSON(path, func(m map[string]any) {
		if n, _ := m["session_start"].(float64); n == 0 {
			if sc := sessionCreated(*name); sc > 0 {
				m["session_start"] = sc
			}
		}
		cur, _ := m["active_seconds"].(float64)
		m["active_seconds"] = int64(cur) + int64(*seconds)
	})
	if err != nil {
		return emitError("write tracking: %v", err)
	}
	// The workspace user owns its home; the agent wrote the file as root.
	_, _ = run("chown", *name+":"+*name, path)
	return emit(agentproto.OK{OK: true})
}

// opUsage reports each workspace's Claude usage: the login it is signed in as, how
// full its context window is, what the session has cost, and where that login
// stands against its 5-hour and weekly limits. Like opActivity it lazily installs
// what produces those numbers — the `forge-usage` command and the statusLine entry
// that runs it — so a workspace made before this existed starts reporting on the
// next poll, with no re-provision.
//
// An entry is emitted if EITHER half is there, and the two are independent. The
// login comes from ~/.claude.json and outlives every session: a stopped workspace
// still answers "which account is this one on", which is half the reason to show it
// at all. The sample comes from the status line and only exists once Claude has
// rendered under our command.
func opUsage() int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return emit(agentproto.UsageResult{Usage: map[string]agentproto.Usage{}})
		}
		return emitError("read %s: %v", baseDir, err)
	}
	res := agentproto.UsageResult{Usage: map[string]agentproto.Usage{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		ensureUsageCmd(name)
		ensureUsageStatusLine(name)
		u, haveSample := readUsage(name)
		account, haveAccount := readAccount(name)
		u.Account = account
		u.Auth = detectAuth(name, haveAccount)
		if taken := statusLineTakenBy(name); taken != "" {
			u.Note = "status line owned by " + taken
		}
		// A workspace paying by credits has no login and no windows, so it would
		// otherwise vanish from the panel entirely — the auth kind is enough to report
		// it, and its context and spend are exactly what such a workspace has to show.
		// A note is reportable on its own for the same reason: it exists precisely
		// where there are no numbers to carry it.
		if haveSample || haveAccount || u.Auth != agentproto.AuthUnknown || u.Note != "" {
			res.Usage[name] = u
		}
	}
	return emit(res)
}

// readUsage reads ~/.claude/forge-usage for a workspace. Absent (the status line
// has not run since the command landed) → not ok.
func readUsage(name string) (agentproto.Usage, bool) {
	data, err := os.ReadFile(filepath.Join(baseDir, name, agentproto.UsageFile))
	if err != nil {
		return agentproto.Usage{}, false
	}
	return parseUsage(data)
}

// parseUsage reads the JSON object usageCmdScript writes. The field names are ours,
// so this decodes straight into the wire type — but it does not trust the contents.
// The file sits in a home directory whose user can write anything into it, and
// these numbers end up in meters and its strings in the browser's DOM, so a sample
// with no timestamp is no sample, text is flattened, and impossible numbers are
// dropped rather than rendered.
func parseUsage(data []byte) (agentproto.Usage, bool) {
	var u agentproto.Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return agentproto.Usage{}, false
	}
	if u.TS <= 0 {
		return agentproto.Usage{}, false
	}
	u.Model = sanitizeText(u.Model, agentproto.LabelMaxRunes)
	u.ContextUsed = atLeastZero(u.ContextUsed)
	u.ContextSize = atLeastZero(u.ContextSize)
	if u.CostUSD < 0 {
		u.CostUSD = 0
	}
	u.FiveHour = sanitizeWindow(u.FiveHour)
	u.SevenDay = sanitizeWindow(u.SevenDay)
	// The login is never taken from this file — the caller reads it from
	// ~/.claude.json — so a sample that tried to name one is ignored.
	u.Account = agentproto.Account{}
	return u, true
}

// sanitizeWindow keeps a rate-limit window inside the range a percentage has. A
// nil window stays nil: absent and 0% are different answers and the pointer is
// what carries the difference.
func sanitizeWindow(w *agentproto.RateWindow) *agentproto.RateWindow {
	if w == nil {
		return nil
	}
	out := *w
	out.UsedPercent = max(0, min(100, out.UsedPercent))
	out.ResetsAt = atLeastZero(out.ResetsAt)
	return &out
}

func atLeastZero(n int64) int64 { return max(0, n) }

// readAccount reads which Claude login a workspace is signed in as, from its
// ~/.claude.json. Absent (no Claude has ever started here) → not ok.
func readAccount(name string) (agentproto.Account, bool) {
	data, err := os.ReadFile(filepath.Join(baseDir, name, agentproto.AccountFile))
	if err != nil {
		return agentproto.Account{}, false
	}
	return parseAccount(data)
}

// parseAccount pulls the signed-in login out of a ~/.claude.json. Only oauthAccount
// is decoded: the rest of that file is Claude Code's own state, none of it ours to
// read or interpret.
//
// No account id means no account. The file is written on the first run, long before
// anyone signs in, so "has a claude.json" and "has a login" are different facts and
// the id is what tells them apart.
func parseAccount(data []byte) (agentproto.Account, bool) {
	var doc struct {
		OAuthAccount struct {
			AccountUUID      string `json:"accountUuid"`
			EmailAddress     string `json:"emailAddress"`
			DisplayName      string `json:"displayName"`
			OrganizationName string `json:"organizationName"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return agentproto.Account{}, false
	}
	got := doc.OAuthAccount
	if got.AccountUUID == "" {
		return agentproto.Account{}, false
	}
	clean := func(s string) string { return sanitizeText(s, agentproto.LabelMaxRunes) }
	return agentproto.Account{
		UUID:  clean(got.AccountUUID),
		Email: clean(got.EmailAddress),
		Name:  clean(got.DisplayName),
		Org:   clean(got.OrganizationName),
	}, true
}

// Claude Code merges settings from several scopes, and two of them outrank the
// user-level file the agent writes. Paths, not constants, so tests can point them
// somewhere writable.
//
// Project scope does not appear here, and that is not an omission: it is
// `.claude/settings.json` read from the directory the session runs in, and Forge
// starts Claude in the workspace home — where that path IS the user file we write.
// A cloned repo's settings only outrank ours for a session started inside the repo,
// which is not the session Forge attaches to.
var (
	managedSettingsPath = "/etc/claude-code/managed-settings.json"
	managedSettingsDir  = "/etc/claude-code/managed-settings.d"
)

// statusLineTakenBy names the higher-precedence scope that defines a status line for
// this workspace's sessions, or "" when ours is the one that runs.
//
// Without this the feature has a silent blind spot. Our entry can be perfectly
// installed and still never execute — an organisation that sets a status line by
// managed policy cannot be overridden by anything, by design — and the workspace
// would simply report no numbers, indistinguishable from one nobody has opened. The
// UI can say "a managed policy owns the status line here"; it cannot guess it.
func statusLineTakenBy(name string) string {
	local := filepath.Join(baseDir, name, ".claude", "settings.local.json")
	if definesStatusLine(local) {
		return "settings.local.json"
	}
	// Managed policy is host-wide, so every workspace on the box is in the same
	// position, and it wins over everything including the command line.
	if definesStatusLine(managedSettingsPath) {
		return "managed policy"
	}
	drops, _ := filepath.Glob(filepath.Join(managedSettingsDir, "*.json"))
	for _, drop := range drops {
		if definesStatusLine(drop) {
			return "managed policy"
		}
	}
	return ""
}

// definesStatusLine reports whether a settings file sets statusLine at all. An
// unreadable or malformed file defines nothing: this decides what to tell the user
// about missing numbers, and a wrong claim there is worse than silence.
func definesStatusLine(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	line, ok := m["statusLine"].(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := line["command"].(string)
	return strings.TrimSpace(cmd) != ""
}

// detectAuth works out how a workspace's Claude pays for itself, so the UI can
// group and label it honestly. A Claude.ai subscription has 5-hour and weekly
// windows; an organisation on API credits, Bedrock or Vertex has none — and those
// two situations otherwise look identical from here, both being a usage sample with
// no rate limits in it.
//
// Provider environment wins over a login on file, because that is the order Claude
// Code itself resolves them in: a workspace can hold a perfectly good oauthAccount
// from a login months ago and still bill every token to Bedrock today. A login is
// the answer only when nothing overrides it.
//
// Only the PRESENCE of a key is ever read, never its value: this ends up in a
// browser panel, and a credential has no business travelling there.
func detectAuth(name string, hasAccount bool) string {
	env, apiKeyHelper := workspaceEnv(name)
	set := func(keys ...string) bool {
		for _, k := range keys {
			if strings.TrimSpace(env[k]) != "" {
				return true
			}
		}
		return false
	}
	switch {
	case truthyEnv(env["CLAUDE_CODE_USE_BEDROCK"]):
		return agentproto.AuthBedrock
	case truthyEnv(env["CLAUDE_CODE_USE_VERTEX"]):
		return agentproto.AuthVertex
	case apiKeyHelper || set("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"):
		return agentproto.AuthAPIKey
	case hasAccount:
		return agentproto.AuthSubscription
	}
	return agentproto.AuthUnknown
}

// truthyEnv reads the on/off flags Claude Code uses for its providers. It accepts
// what a person would plausibly write, and treats an explicit "0"/"false" as off —
// a variable set to zero is a decision, not a switch.
func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// workspaceEnv collects the environment a workspace's Claude runs with, from the two
// places a Forge workspace can hold one: ~/.forge/env, which every session sources
// (see writeEnvFile), and the `env` block of ~/.claude/settings.json, which Claude
// Code applies itself. It also reports whether settings.json has an apiKeyHelper,
// which is the third way to hand Claude a key.
//
// A key exported from an interactive ~/.bashrc is not seen, and deliberately so:
// the answer has to be the same whoever asks and whenever, and that file's effect
// depends on how the shell was started. Both files here are read the same way every
// time.
func workspaceEnv(name string) (map[string]string, bool) {
	home := filepath.Join(baseDir, name)
	env := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(home, envRelPath)); err == nil {
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	var apiKeyHelper bool
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
		var doc struct {
			Env          map[string]string `json:"env"`
			APIKeyHelper string            `json:"apiKeyHelper"`
		}
		if json.Unmarshal(data, &doc) == nil {
			for k, v := range doc.Env {
				env[k] = v
			}
			apiKeyHelper = strings.TrimSpace(doc.APIKeyHelper) != ""
		}
	}
	return env, apiKeyHelper
}

// usageCmdRelPath is where the `forge-usage` command lives in a workspace home,
// beside `forge-topic` and for the same reason: ~/.local/bin is where the Claude
// install already puts things.
const usageCmdRelPath = ".local/bin/forge-usage"

// usageStatusLinePrefix is the command settings.json runs, before any status line
// of the user's chained onto it. Spelled with $HOME rather than as a bare name on
// PATH: PATH does carry ~/.local/bin (see writeEnvFile), but only for a session
// started through the workspace env file, and a status line that quietly does
// nothing in the other case would be very hard to notice.
const usageStatusLinePrefix = `"$HOME/` + usageCmdRelPath + `"`

// usageStatusLine builds the command, keeping tail — an already shell-quoted
// argument holding a status line the workspace had before us. Claude's status line
// is a single slot, so taking it over is the only way in; passing the old command
// along as an argument, for our script to run and print, is what makes that a
// takeover rather than a loss. The tail travels verbatim once quoted, so carrying it
// across our own upgrades costs no unquoting and cannot corrupt it.
func usageStatusLine(tail string) string {
	if tail == "" {
		return usageStatusLinePrefix
	}
	return usageStatusLinePrefix + " " + tail
}

// usageRefreshSeconds re-runs the status line on a timer as well as on session
// events. It is the whole reason the rate-limit numbers can be trusted: the event
// triggers go quiet exactly when nothing is happening, and a workspace sitting idle
// would otherwise keep reporting the window it saw when it last spoke — "23%",
// hours stale, is the reading you least want to believe. Thirty seconds is far
// finer than a five-hour window moves, and the script it spawns is tiny.
const usageRefreshSeconds = 30

// usageCmdScript is the status line command itself. It has two jobs at once, which
// is what makes this feature possible without a second moving part: Claude Code
// hands a statusLine command a JSON snapshot of the session on stdin — the only
// place the 5-hour and weekly figures exist, since nothing on disk records them —
// and prints whatever the command writes. So this files what Forge needs for the
// agent to collect, and prints a summary so the same numbers are visible to
// somebody sitting in the tmux session.
//
// Every part of the parse is defensive and independent. This runs on every render
// of somebody's live session; a field Claude Code adds, renames or omits must cost
// at most the one number that came from it, never the status line.
const usageCmdScript = `#!/usr/bin/env python3
# forge-usage — the Claude Code status line Forge installs in each workspace.
# It writes ~/` + agentproto.UsageFile + ` for the Forge agent to read, and prints
# a one-line summary for whoever is looking at the session.
#
# A status line the workspace already had is passed to us as our first argument and
# still gets to print, above our own row: the slot is single, the screen is not.
import json, os, subprocess, sys, tempfile, time

raw = sys.stdin.read()
try:
    d = json.loads(raw or "{}")
except Exception:
    d = {}

def obj(x, k):
    v = x.get(k) if isinstance(x, dict) else None
    return v if isinstance(v, dict) else {}

# bool is an int in python, and "true" is not a measurement.
def num(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) else 0

cw = obj(d, "context_window")

def window(key):
    w = obj(obj(d, "rate_limits"), key)
    p = w.get("used_percentage")
    # Absent is not zero. A login with no window this session has not called the
    # API yet; reporting 0% would read as "plenty left".
    if not isinstance(p, (int, float)) or isinstance(p, bool):
        return None
    return {"used_percent": float(p), "resets_at": int(num(w.get("resets_at")))}

sample = {
    "ts": int(time.time()),
    "model": str(obj(d, "model").get("display_name") or ""),
    "context_used": int(num(cw.get("total_input_tokens"))),
    "context_size": int(num(cw.get("context_window_size"))),
    "cost_usd": float(num(obj(d, "cost").get("total_cost_usd"))),
    "five_hour": window("five_hour"),
    "seven_day": window("seven_day"),
}

path = os.path.join(os.path.expanduser("~"), "` + agentproto.UsageFile + `")
try:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    # Through a temp file and renamed: the agent polls this path on its own
    # schedule, and half a JSON object is not a sample it can read.
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
    with os.fdopen(fd, "w") as f:
        json.dump(sample, f)
    os.replace(tmp, path)
except Exception:
    pass  # the status line still has a line to print

# Theirs first, and given the same stdin we were given — a status line command is
# entitled to the session snapshot, not to whatever we left of it. Its failure is
# not ours to report: a bad command of theirs costs its own row and nothing else.
if len(sys.argv) > 1 and sys.argv[1].strip():
    try:
        r = subprocess.run(["sh", "-c", sys.argv[1]], input=raw,
                           capture_output=True, text=True, timeout=5)
        theirs = r.stdout.rstrip("\n")
        if theirs:
            print(theirs)
    except Exception:
        pass

bits = []
if sample["model"]:
    bits.append(sample["model"])
if sample["context_size"]:
    bits.append("ctx %d%%" % round(100.0 * sample["context_used"] / sample["context_size"]))
for label, key in (("5h", "five_hour"), ("7d", "seven_day")):
    w = sample[key]
    if w:
        bits.append("%s %d%%" % (label, round(w["used_percent"])))
print(" · ".join(bits))
`

// seedUsageCmd installs that script into a workspace home, owned by the workspace
// user — the same shape as seedTopicCmd, which explains why the ownership matters.
func seedUsageCmd(home, name string) error {
	if err := writeUsageCmd(home); err != nil {
		return err
	}
	path := filepath.Join(home, usageCmdRelPath)
	if out, err := run("chown", name+":"+name, path, filepath.Dir(path)); err != nil {
		return fmt.Errorf("chown usage cmd: %v: %s", err, out)
	}
	return nil
}

// writeUsageCmd writes the script and its directory; split out so it can be tested
// without root, the same split as writeTopicCmd.
func writeUsageCmd(home string) error {
	path := filepath.Join(home, usageCmdRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(usageCmdScript), 0o755)
}

// ensureUsageCmd installs the command into a workspace that predates it, or whose
// copy is out of date — same lazy backfill, on the same sweep, as ensureTopicCmd.
func ensureUsageCmd(name string) {
	home := filepath.Join(baseDir, name)
	if data, err := os.ReadFile(filepath.Join(home, usageCmdRelPath)); err == nil &&
		string(data) == usageCmdScript {
		return
	}
	_ = seedUsageCmd(home, name)
}

// ensureUsageStatusLine points a workspace's settings.json at that command, for
// workspaces that predate this. Best-effort and quiet, like ensureActivityHooks.
//
// A status line the workspace already had is not discarded but chained: ours runs,
// files the sample, then runs theirs and prints its output above our row. The slot
// is single — unlike hooks, which is why setActivityHooks can simply append — but
// the screen is not, and a workspace that reported nothing because somebody liked
// their prompt was the wrong trade.
func ensureUsageStatusLine(name string) {
	settings := filepath.Join(baseDir, name, ".claude", "settings.json")
	tail := ""
	if data, err := os.ReadFile(settings); err == nil {
		var m map[string]any
		// A settings.json we cannot parse is one mergeJSON would replace wholesale.
		// Leave it: the file is the user's, and losing it would cost more than this
		// workspace's usage numbers are worth.
		if json.Unmarshal(data, &m) != nil {
			return
		}
		if line, ok := m["statusLine"].(map[string]any); ok {
			raw, _ := line["command"].(string) // a non-string command is no command
			cmd := strings.TrimSpace(raw)
			switch {
			case strings.HasPrefix(cmd, usageStatusLinePrefix):
				if usageStatusLineCurrent(line) {
					return // already ours, already current: no write at all
				}
				// Ours, but out of date. Whatever we chained last time is still theirs
				// and still quoted, so it comes along unexamined.
				tail = strings.TrimSpace(strings.TrimPrefix(cmd, usageStatusLinePrefix))
			case cmd != "":
				tail = agentproto.ShellQuote(cmd) // theirs: take the slot, keep the line
			}
		}
	}
	// ~/.claude normally exists by now (the Claude install and seedClaudeConfig both
	// make it), but a workspace whose home was rebuilt by hand would otherwise lose
	// this quietly and for good — the sweep would retry forever against a missing
	// directory.
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		return
	}
	if err := mergeJSON(settings, func(m map[string]any) { setUsageStatusLine(m, tail) }); err != nil {
		return
	}
	// The workspace user owns its config; the agent runs as root. The directory too,
	// in case the line above is what created it.
	_, _ = run("chown", name+":"+name, filepath.Dir(settings), settings)
}

// setUsageStatusLine writes the statusLine entry, chaining tail (an already
// shell-quoted status line of the user's) when there is one.
func setUsageStatusLine(m map[string]any, tail string) {
	m["statusLine"] = map[string]any{
		"type":            "command",
		"command":         usageStatusLine(tail),
		"refreshInterval": usageRefreshSeconds,
	}
}

// usageStatusLineCurrent reports whether an existing (ours) statusLine entry is
// what we would write today. It checks the interval rather than the whole command,
// because the command's tail is the user's and is carried forward as found —
// comparing that too would rewrite the file forever over a difference we put there
// on purpose. Checking the interval at all is what lets a change to it reach every
// workspace on the next sweep, the way ensureUsageCmd compares the script.
func usageStatusLineCurrent(line map[string]any) bool {
	cmd, _ := line["command"].(string)
	if !strings.HasPrefix(strings.TrimSpace(cmd), usageStatusLinePrefix) {
		return false
	}
	// JSON numbers decode as float64; a settings file written by hand may hold an
	// int-looking value either way.
	switch n := line["refreshInterval"].(type) {
	case float64:
		return int(n) == usageRefreshSeconds
	case int:
		return n == usageRefreshSeconds
	}
	return false
}

// cpuWindow is how long host-stats watches /proc/stat before reporting a
// percentage. Long enough that a scheduler tick or two lands in it and the number
// is not pure noise; short enough that the poll behind it stays much cheaper than
// the SSH round trip that carried it.
const cpuWindow = 200 * time.Millisecond

// opHostStats reports the server's own resource usage: CPU, memory and the disk
// the workspaces live on.
//
// Every part is best-effort and independent, because a machine that cannot answer
// one of these can usually answer the others, and a panel showing two of three
// numbers beats one showing an error. Something unmeasurable is left at zero,
// which is unambiguous for each of them — a real host has neither zero cores nor
// zero bytes of RAM — so the browser can say "—" rather than a confident lie. That
// includes CPU: 0% is a plausible reading on an idle box, so the *cores* count
// (from the same file) is what says whether the CPU figure means anything.
func opHostStats() int {
	st := agentproto.HostStats{}
	if pct, cores, ok := cpuUsage(cpuWindow); ok {
		st.CPUPercent, st.CPUCores = pct, cores
	}
	if data, err := os.ReadFile(filepath.Join(procRoot, "meminfo")); err == nil {
		if total, used, ok := parseMemInfo(data); ok {
			st.MemTotal, st.MemUsed = total, used
		}
	}
	st.DiskPath = diskPath()
	if total, used, ok := diskUsage(st.DiskPath); ok {
		st.DiskTotal, st.DiskUsed = total, used
	}
	if data, err := os.ReadFile(filepath.Join(procRoot, "uptime")); err == nil {
		st.Uptime = parseUptime(data)
	}
	return emit(st)
}

// cpuUsage samples /proc/stat twice, `window` apart, and returns the busy share
// of all cores across that interval — plus the number of cores, which doubles as
// "was this measurable at all".
//
// Two samples are the point. /proc/stat holds counters since boot, so a single
// read yields the average since the machine was switched on: a server that has
// been up for a month and is on fire right now would report a calm 4%.
func cpuUsage(window time.Duration) (percent float64, cores int, ok bool) {
	first, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, 0, false
	}
	idle1, total1, cores, ok := parseCPUStat(first)
	if !ok {
		return 0, 0, false
	}
	time.Sleep(window)
	second, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, 0, false
	}
	idle2, total2, _, ok := parseCPUStat(second)
	if !ok {
		return 0, 0, false
	}
	dTotal, dIdle := total2-total1, idle2-idle1
	if total2 < total1 || dTotal == 0 {
		// Counters only ever climb, so a smaller second read means we were handed
		// something that isn't /proc/stat. An unchanged one means the window closed
		// inside a single tick — the cores are real, the percentage isn't.
		return 0, cores, true
	}
	busy := float64(dTotal-dIdle) / float64(dTotal) * 100
	return max(0, min(100, busy)), cores, true
}

// parseCPUStat reads /proc/stat's aggregate "cpu" line into (idle, total) jiffies
// and counts the per-core "cpuN" lines.
//
// idle counts iowait as idle — a core waiting on a disk is not doing work, and
// calling that "busy" is how a box with one slow disk reports 100% CPU. total sums
// user..steal only: guest and guest_nice are already included in user and nice, so
// adding them again inflates the denominator and quietly understates usage.
func parseCPUStat(data []byte) (idle, total uint64, cores int, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cores++ // cpu0, cpu1, …
			continue
		}
		if len(f) < 5 {
			continue // too short to hold even idle: not a line we can use
		}
		for i, field := range f[1:] {
			if i >= 8 {
				break // guest / guest_nice: already counted in user / nice
			}
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, 0, 0, false
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		ok = true
	}
	return idle, total, cores, ok
}

// parseMemInfo reads /proc/meminfo into total and used bytes.
//
// Used is total minus MemAvailable, not minus MemFree: Linux spends every spare
// byte on page cache, so MemFree on a healthy server is near zero and "used" from
// it reads as 97% on a machine with plenty of room. MemAvailable is the kernel's
// own estimate of what a new process could actually get, which is the number a
// person means. Kernels too old to publish it (pre-3.14) fall back to
// free+buffers+cached.
func parseMemInfo(data []byte) (total, used uint64, ok bool) {
	vals := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		vals[key] = n * 1024 // every value we want is in kB
	}
	total = vals["MemTotal"]
	if total == 0 {
		return 0, 0, false
	}
	avail, hasAvail := vals["MemAvailable"]
	if !hasAvail {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	if avail > total {
		avail = total
	}
	return total, total - avail, true
}

// diskPath is the filesystem host-stats measures: the one holding the workspaces,
// which is the disk that fills up and the one you can do something about. A host
// where that directory doesn't exist yet (nothing created on it) is measured at
// the root instead.
func diskPath() string {
	if _, err := os.Stat(baseDir); err == nil {
		return baseDir
	}
	return "/"
}

// diskUsage returns a filesystem's total and used bytes.
//
// Used counts the root-reserved blocks that `df` leaves out of its percentage, so
// this can read a few percent fuller than df does on an ext4 with the default 5%
// reservation. That is the honest direction to differ in: the reserve is real
// space an ordinary process cannot have.
func diskUsage(path string) (total, used uint64, ok bool) {
	bsize, blocks, bfree, err := statfs(path)
	if err != nil || bsize == 0 || blocks == 0 {
		return 0, 0, false
	}
	if bfree > blocks {
		return 0, 0, false
	}
	return blocks * bsize, (blocks - bfree) * bsize, true
}

// parseUptime reads the seconds since boot from /proc/uptime ("<up> <idle>").
func parseUptime(data []byte) int64 {
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return int64(secs)
}

// ensureActivityHooks installs the activity hooks into a workspace that predates
// them. Idempotent and cheap: the marker check short-circuits once seeded, so the
// write (and chown) happens at most once per workspace. Best-effort — a status
// poll must not fail because one workspace's config is odd or unreadable.
func ensureActivityHooks(name string) {
	settings := filepath.Join(baseDir, name, ".claude", "settings.json")
	// Our current hooks are the only thing mentioning ALL of the activity file, the
	// background_tasks gate and the topic file, so requiring all three as the marker
	// won't collide with an unrelated user hook that happens to contain one word —
	// and still forces an upgrade from each earlier version: the first was ungated
	// (forge-activity, no gate), the second didn't nudge for a topic.
	if data, err := os.ReadFile(settings); err == nil {
		s := string(data)
		if strings.Contains(s, "forge-activity") && strings.Contains(s, "background_tasks") &&
			strings.Contains(s, "forge-topic") {
			return
		}
	}
	if err := mergeJSON(settings, setActivityHooks); err != nil {
		return
	}
	// The workspace user owns its config; the agent runs as root.
	_, _ = run("chown", name+":"+name, settings)
}

// The hooks write a "<state> <unix-seconds>" line to ~/.claude/forge-activity —
// the same path readActivity reads. They run under a shell with $HOME set to the
// workspace user's home, and the mkdir keeps them working even if ~/.claude was
// removed.
//
// busyHookCmd fires on UserPromptSubmit: you just gave Claude work, so it's
// unambiguously busy — no need to inspect anything.
//
// It has a second job: making the workspace topic set itself. A UserPromptSubmit
// hook's stdout is added to the conversation as context, so this is where Claude
// can be *told* to run `forge-topic` — deterministically, on a schedule the model
// doesn't get a vote on, rather than hoping it remembers a standing instruction.
// The nudge is what makes the feature autonomous; nobody types a topic by hand.
//
// It is gated, or it would be nagging. The topic is stale exactly when it predates
// the session it is supposed to describe, so the gate compares it against the
// session start — and takes that start the same way opTrack does: the tracking
// file if it exists, the tmux session's own creation time otherwise. That is what
// gives a checkpoint the behaviour it needs for free. A checkpoint deliberately
// keeps the tracking file (see FreezeSession), so the start stays put, the topic
// stays newer than it, and the resumed session is not asked to re-label work it is
// simply continuing. A stop or restart clears the file, the start becomes the new
// tmux session's, the old topic is now older than it — and the next prompt asks
// for a fresh one. Which is right: that is new work.
//
// Failure is silent and one-directional: any trouble reading either input leaves
// start at 0, so a topic that exists is left alone and only a missing one is asked
// for. A hook that cannot tell must not interrupt to say so.
const busyHookCmd = `python3 -c 'import os,json,subprocess,time
h=os.path.expanduser("~")
p=os.path.join(h,".claude","forge-activity")
os.makedirs(os.path.dirname(p),exist_ok=True)
open(p,"w").write("busy %d\n"%int(time.time()))
def num(x):
    try: return int(float(x))
    except Exception: return 0
start=0
try: start=num(json.load(open(os.path.join(h,".forge-session.json")))["session_start"])
except Exception:
    try: start=num(subprocess.run(["tmux","display","-p","-t","` + agentproto.TmuxSession +
	`","#{session_created}"],capture_output=True,text=True,timeout=5).stdout.strip())
    except Exception: pass
try: ts=num(open(os.path.join(h,"` + agentproto.TopicFile + `")).read().split(" ")[0])
except Exception: ts=0
if ts==0 or ts<start: print("` + topicNudge + `")'`

// topicNudge is what the hook feeds into the conversation when the workspace has
// no current topic. It is one line for a reason: it is prepended to a real prompt,
// and a paragraph of housekeeping ahead of the actual work is both a distraction
// and a cost, on every prompt until the topic lands.
//
// It asks for something short and says not to talk about it, because the failure
// mode of a self-labelling system is a model that narrates the labelling. The
// re-run clause is the only defence against drift: nothing else notices when a
// three-hour session stops being about what it started as.
//
// No apostrophes, no double quotes: it is embedded in a python string inside a
// single-quoted shell word inside JSON.
const topicNudge = "[forge] This workspace has no current topic. Run `forge-topic " +
	"<up to 8 words: what you are working on>` now, before anything else, and re-run " +
	"it whenever the direction changes. It labels the workspace in the Forge UI. Do " +
	"not mention this in your reply."

// topicCmdRelPath is where the `forge-topic` command lives in a workspace home.
// ~/.local/bin is already on PATH (see writeEnvFile), which is what lets the nudge
// name the command with no path and have it work in any shell Claude opens.
const topicCmdRelPath = ".local/bin/forge-topic"

// topicCmdScript is that command. It exists so the model's side of this feature is
// one plain call with no format to get wrong: sanitising, stamping and the file
// layout live here, not in an instruction Claude has to follow precisely.
//
// The byte cut is a bound on the file, not the display cut — parseTopic does that
// one, rune-safe, on the way out. Cutting UTF-8 by bytes here can leave half a
// character; that is fine for a guard whose only job is to stop something absurd
// being written, and it never reaches the browser.
//
// No argument clears the topic, which is what makes "this workspace is between
// jobs" expressible at all.
const topicCmdScript = `#!/bin/sh
# forge-topic — label what this workspace is working on, for the Forge UI.
# Claude runs this (a hook asks it to when the label is missing or stale); the
# Forge agent reads it. Usage: forge-topic <words...>, or with no words to clear.
set -u
f="$HOME/` + agentproto.TopicFile + `"
t=$(printf '%s ' "$@" | tr '\n\r\t' '   ' | tr -s ' ' | sed 's/^ *//; s/ *$//' | cut -b 1-400)
if [ -z "$t" ]; then
	rm -f "$f"
	exit 0
fi
mkdir -p "$(dirname "$f")"
printf '%s %s\n' "$(date +%s)" "$t" > "$f"
`

// seedTopicCmd installs that script into a workspace home, owned by the workspace
// user (the agent runs as root, so an unwritable file would be worse than none).
func seedTopicCmd(home, name string) error {
	if err := writeTopicCmd(home); err != nil {
		return err
	}
	path := filepath.Join(home, topicCmdRelPath)
	if out, err := run("chown", name+":"+name, path, filepath.Dir(path)); err != nil {
		return fmt.Errorf("chown topic cmd: %v: %s", err, out)
	}
	return nil
}

// writeTopicCmd writes the script and its directory; split out so it can be tested
// without root, the same split as writeClaudeConfig.
func writeTopicCmd(home string) error {
	path := filepath.Join(home, topicCmdRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(topicCmdScript), 0o755)
}

// ensureTopicCmd installs the command into a workspace that predates it, or whose
// copy is out of date — the same lazy backfill as ensureActivityHooks, on the same
// sweep. Comparing contents rather than just existence means a change to the script
// reaches every workspace on the next poll, without a re-provision. Best-effort: a
// status poll must not fail over one workspace's home.
func ensureTopicCmd(name string) {
	home := filepath.Join(baseDir, name)
	if data, err := os.ReadFile(filepath.Join(home, topicCmdRelPath)); err == nil &&
		string(data) == topicCmdScript {
		return
	}
	_ = seedTopicCmd(home, name)
}

// gatedHookCmd fires on Stop/Notification: the turn ended (or Claude notified),
// but that is NOT the same as "waiting for you". If Claude left background work
// running — a background shell or a background subagent — it will resume on its
// own, so the tab must not light up. Claude Code hands the hook a background_tasks
// list on stdin (each entry has a "status"); if anything there is still running we
// report busy, otherwise the real end-state. This is what kills the false positive
// during multi-agent orchestration, where Stop/Notification fire repeatedly while
// agents are still churning.
func gatedHookCmd(endState string) string {
	return `python3 -c 'import sys,json,time,os
try: d=json.loads(sys.stdin.read() or "{}")
except Exception: d={}
bt=d.get("background_tasks")
r=any(isinstance(t,dict) and t.get("status")=="running" for t in bt) if isinstance(bt,list) else False
p=os.path.expanduser("~/.claude/forge-activity")
os.makedirs(os.path.dirname(p),exist_ok=True)
open(p,"w").write(("busy" if r else "` + endState + `")+" %d\n"%int(time.time()))'`
}

// setActivityHooks installs the Claude Code hooks that report attention state.
// UserPromptSubmit → busy; Stop → idle (unless background work is running);
// Notification → waiting (same gate). Each stamps the current second so the UI can
// dismiss a seen episode and light up again on the next one.
//
// Forge's matcher is APPENDED to any hooks the user already has for that event,
// never replacing them — a workspace user's own Stop hook keeps running. Re-seeding
// stays idempotent: a forge matcher from a previous run is dropped before ours is
// re-appended, so the list can't grow a duplicate each time.
func setActivityHooks(m map[string]any) {
	hooks := childMap(m, "hooks")
	install := func(event, command string) {
		ours := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}
		var kept []any
		if existing, ok := hooks[event].([]any); ok {
			for _, e := range existing {
				if !isForgeActivityMatcher(e) {
					kept = append(kept, e)
				}
			}
		}
		hooks[event] = append(kept, ours)
	}
	install("UserPromptSubmit", busyHookCmd)
	install("Stop", gatedHookCmd(agentproto.ActivityIdle))
	install("Notification", gatedHookCmd(agentproto.ActivityWaiting))
}

// isForgeActivityMatcher reports whether a hook matcher is one WE wrote — its
// command touches the activity file. Used to drop a stale forge matcher before
// re-appending, so re-seeding doesn't accumulate duplicates.
func isForgeActivityMatcher(e any) bool {
	matcher, ok := e.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := matcher["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, "forge-activity") {
			return true
		}
	}
	return false
}

func seedSSH(home, name string, pubkey []byte) error {
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	authKeys := filepath.Join(sshDir, "authorized_keys")
	data := pubkey
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	return os.WriteFile(authKeys, data, 0o600)
}

// hostKeyDir holds the host-wide git identity created by `forge host prepare`.
// hostGhDir holds the host-wide gh credential created by `forge host gh-login`.
// Both are copied into each workspace at create. Kept in sync with internal/cli.
const (
	hostKeyDir = "/etc/forge"
	hostGhDir  = hostKeyDir + "/gh"
)

// seedGhAuth copies the host's gh credential into the workspace, so gh works
// there without logging in again. gh reads ~/.config/gh/hosts.yml; one login per
// host beats one per workspace, and separate tokens would buy nothing on a box
// where every workspace user can already read the others' files.
//
// hosts.yml holds an OAuth token, so it is written 0600; the caller's chown hands
// it to the workspace user. A host with no gh login is not an error — the
// workspace simply has no gh credential until `forge host gh-login` runs.
func seedGhAuth(home, ghDir string) error {
	data, err := os.ReadFile(filepath.Join(ghDir, "hosts.yml"))
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: no gh login at %s — run `forge host gh-login <alias>` to add one\n", ghDir)
		return nil
	}
	if err != nil {
		return err
	}
	dst := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dst, "hosts.yml"), data, 0o600)
}

// seedGitKey copies the host's git identity into the workspace, so git works
// with no forwarded agent. A forwarded agent cannot serve the Claude session:
// tmux outlives the SSH connection that started it, so the forwarded socket is
// dead by the time Claude pushes — and dead for good once the laptop is off,
// which is the case Forge exists for.
//
// The key is copied, not shared through a group-readable path, so git finds it at
// the default ~/.ssh/id_ed25519 with no GIT_SSH_COMMAND or ssh config, and
// deleting the workspace takes its copy with it. Every workspace on the host gets
// the same identity; that matches the boundary Forge draws (workspace users are
// in the docker group, so they can already reach each other's files).
//
// A host prepared before this existed has no key: that is not an error, the
// workspace just has no git credentials until the host is re-prepared.
func seedGitKey(home, keyDir string) error {
	priv, err := os.ReadFile(filepath.Join(keyDir, "id_ed25519"))
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: no git identity at %s — re-run `forge host prepare` to create one\n", keyDir)
		return nil
	}
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), priv, 0o600); err != nil {
		return err
	}
	// The .pub and known_hosts are conveniences, not credentials: copy when present.
	for _, f := range []string{"id_ed25519.pub", "known_hosts"} {
		data, err := os.ReadFile(filepath.Join(keyDir, f))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(sshDir, f), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// envRelPath is the workspace-local environment file (relative to the user's
// home). It holds KEY=value lines and is the single source of truth for the
// workspace's environment (COMPOSE_PROJECT_NAME today, more later). It is
// sourced with `set -a` so every entry becomes exported.
//
// It exists because .bashrc alone is unreliable: .bashrc is sourced only by
// interactive shells (and most distros' .bashrc returns early for
// non-interactive ones), so `docker compose` run non-interactively — a script,
// `bash -c`, a `make` target — would miss the variable. Instead we keep the
// values in this file and source it both from .bashrc (interactive shells) and
// from Forge's own launch commands (the Claude/tmux session), covering every
// invocation path.
const envRelPath = ".forge/env"

// writeEnvFile creates the workspace environment file.
func writeEnvFile(home, name string) error {
	dir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// COMPOSE_PROJECT_NAME scopes the compose project (and, in tooling that keys
	// its network name off it, the docker network too) to this workspace — so
	// parallel clones stay isolated. PATH includes ~/.local/bin, where the native
	// Claude Code installer puts the `claude` binary. CLAUDE_REMOTE_CONTROL_...
	// names the Remote Control session after the workspace instead of the default
	// hostname — it's the *prefix* that Claude shows in the app (not --name), so
	// sessions read as `marbai-01`, `marbai-02`… rather than `hostname-random`.
	// Host ports live in each repo's own .env.
	content := fmt.Sprintf(
		"COMPOSE_PROJECT_NAME=%[1]s\n"+
			"PATH=$HOME/.local/bin:$PATH\n"+
			"CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX=%[1]s\n",
		name)
	return os.WriteFile(filepath.Join(home, envRelPath), []byte(content), 0o644)
}

// forgeBashrcBlock is appended to the workspace user's .bashrc. %[1]s is the
// workspace name. It (a) sources the workspace environment file so interactive
// shells get COMPOSE_PROJECT_NAME et al., and (b) shadows the `claude` binary
// for interactive shells so a stray launch (which would die on disconnect) is
// redirected to the managed flow.
const forgeBashrcBlock = `
# --- forge: workspace environment ---
set -a; [ -f "$HOME/.forge/env" ] && . "$HOME/.forge/env"; set +a

claude() {
  echo "⚠  Claude runs managed via tmux so it survives disconnects."
  echo "   Use:  forge workspace %[1]s claude"
}
# --- end forge ---
`

func seedBashrc(home, name string) error {
	f, err := os.OpenFile(filepath.Join(home, ".bashrc"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, forgeBashrcBlock, name)
	return err
}

func seedGitconfig(home string) error {
	const cfg = "[init]\n\tdefaultBranch = main\n[pull]\n\trebase = false\n"
	return os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644)
}

// tmuxConf makes a workspace session feel like a plain terminal, and makes text
// in it copyable from a laptop hundreds of miles away.
//
// No status bar: no green line telling you where you are; you already know.
//
// mouse on is what makes copy work. Without it tmux ignores the drag and the
// terminal tries to select for itself — but Claude runs on tmux's alternate
// screen, where some terminals (Warp) draw a highlight they then refuse to copy:
// you select, and Cmd-C does nothing. With mouse on, tmux owns the drag, enters
// copy-mode, and copy-selection-and-cancel puts the text on *your* clipboard via
// the OSC 52 escape, which travels back over SSH. It also gives you wheel
// scrollback, which an alternate-screen session otherwise has none of.
//
// The cost: the terminal's own selection now needs Shift (or Option) held down,
// because plain drags belong to tmux.
//
// set-clipboard on rather than the default external: both set your clipboard on a
// yank, but on also lets Claude itself put things there.
const tmuxConf = `set -g status off
set -g mouse on
set -g set-clipboard on
set -as terminal-features ',*:clipboard'
bind -T copy-mode    MouseDragEnd1Pane send -X copy-selection-and-cancel
bind -T copy-mode-vi MouseDragEnd1Pane send -X copy-selection-and-cancel
`

func seedTmuxConf(home string) error {
	return os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte(tmuxConf), 0o644)
}

// seedClaudeConfig pre-answers the two things Claude would otherwise stop and ask
// a human, in the two files that hold them.
//
// ~/.claude.json: the folder trust dialog, which does not persist reliably when
// accepted interactively and so reappears every launch.
//
// ~/.claude/settings.json: bypassPermissions, so tool calls run without an
// approval prompt. This is deliberate and it is the point of a workspace — you
// drive it from a phone, or from nothing at all while the laptop sleeps, and
// there is nobody there to type "yes". The blast radius is the workspace: an
// unprivileged Linux user, on a box whose only inbound port is SSH. Note it is
// still a real grant — Claude can run any command as that user, and the docker
// group means that reaches the host. Do not put anything on a Forge host you
// would not hand to Claude.
//
// Claude refuses bypassPermissions when running as root; workspaces are not, so
// this holds.
func seedClaudeConfig(home, name string) error {
	if err := writeClaudeConfig(home); err != nil {
		return err
	}
	claudeJSON := filepath.Join(home, ".claude.json")
	claudeDir := filepath.Join(home, ".claude")
	if out, err := run("chown", "-R", name+":"+name, claudeJSON, claudeDir); err != nil {
		return fmt.Errorf("chown claude config: %v: %s", err, out)
	}
	return nil
}

// writeClaudeConfig writes the two files; seedClaudeConfig then owns them by the
// workspace user. Split out so it can be tested without root.
func writeClaudeConfig(home string) error {
	if err := mergeJSON(filepath.Join(home, ".claude.json"), func(m map[string]any) {
		childMap(childMap(m, "projects"), home)["hasTrustDialogAccepted"] = true
	}); err != nil {
		return err
	}
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return err
	}
	return mergeJSON(filepath.Join(claudeDir, "settings.json"), func(m map[string]any) {
		childMap(m, "permissions")["defaultMode"] = "bypassPermissions"
		setActivityHooks(m)       // report Claude's attention state to the UI
		setUsageStatusLine(m, "") // report its context, cost and rate limits too
	})
}

// mergeJSON reads a JSON object (or starts empty / on a malformed file), applies
// fn, and writes it back pretty-printed.
func mergeJSON(path string, fn func(map[string]any)) error {
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &m) // tolerate garbage: start fresh
	}
	fn(m)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// childMap returns m[key] as a map, creating it if missing or not an object.
func childMap(m map[string]any, key string) map[string]any {
	if child, ok := m[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	m[key] = child
	return child
}

func writeMetadata(home, name string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	meta := map[string]string{
		"name":         name,
		"owner":        name,
		"tmux_session": agentproto.TmuxSession,
		"created_at":   now,
		"last_used":    now,
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, metadataFile), data, 0o644)
}

// migrateMetadata renames a workspace's pre-rename visible workspace.json to the
// hidden .workspace.json. Idempotent and best-effort: it skips once the dotfile
// exists and does nothing if there is no old file, so a status sweep never fails on
// it. Safe because nothing reads the metadata at runtime — the rename keeps the same
// inode and owner, it only stops the file cluttering the file tree.
func migrateMetadata(name string) {
	dot := filepath.Join(baseDir, name, metadataFile)
	if _, err := os.Stat(dot); err == nil {
		return
	}
	old := filepath.Join(baseDir, name, "workspace.json")
	if _, err := os.Stat(old); err != nil {
		return
	}
	_ = os.Rename(old, dot)
}

// run executes a command and returns combined output for error context.
func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// tailLines returns the last n lines of s (verbose installer output is trimmed
// to something readable in an error message).
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func emit(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return emitError("marshal: %v", err)
	}
	fmt.Println(string(data))
	return 0
}

func emitError(format string, a ...any) int {
	data, _ := json.Marshal(agentproto.ErrorResult{Error: fmt.Sprintf(format, a...)})
	fmt.Println(string(data))
	return 1
}
