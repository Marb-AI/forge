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

	"github.com/Marb-AI/forge/internal/agentproto"
)

// baseDir is where workspace home directories live.
const baseDir = "/home/workspaces"

// nameRe restricts workspace names to safe Linux usernames — these become paths
// and command arguments, so we validate strictly.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Main is the forge-agent entrypoint; returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		return emitError("usage: forge-agent <workspace-create|workspace-delete|workspace-list|workspace-status>")
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

// reapKill is how long we give the user's processes to exit after a signal —
// once after SIGTERM (so a shell or Claude can wind down), once after SIGKILL.
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
// SIGTERM first, SIGKILL for whatever ignores it. An unknown user is not an error
// — there is nothing to reap, and userdel will say so better than we can.
func reapUser(name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return nil
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
	return os.WriteFile(filepath.Join(home, "workspace.json"), data, 0o644)
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
