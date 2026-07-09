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
	"path/filepath"
	"regexp"
	"strings"
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

	// Pre-configure Claude so a session starts cleanly and shows up in the app:
	// pre-trust the workspace folder (its trust dialog otherwise reappears every
	// launch — it doesn't persist reliably when accepted interactively) and enable
	// Remote Control by default.
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
	if out, err := run("userdel", "-r", *name); err != nil {
		return emitError("userdel: %v: %s", err, out)
	}
	return emit(agentproto.OK{OK: true})
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

// seedTmuxConf turns off the tmux status bar so a workspace session feels like a
// plain terminal — no green bar telling you where you are; you already know.
func seedTmuxConf(home string) error {
	return os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte("set -g status off\n"), 0o644)
}

// seedClaudeConfig pre-trusts the workspace folder so Claude's trust dialog never
// appears (accepted interactively it doesn't persist). It deliberately does NOT
// auto-enable Remote Control: doing so risks Claude offering to resume a killed
// session, and a killed session (/exit, Ctrl+C) must stay closed. Remote Control
// is a manual `/remote-control` inside the session instead — named after the
// workspace via CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX in the env.
func seedClaudeConfig(home, name string) error {
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := mergeJSON(claudeJSON, func(m map[string]any) {
		childMap(childMap(m, "projects"), home)["hasTrustDialogAccepted"] = true
	}); err != nil {
		return err
	}
	if out, err := run("chown", name+":"+name, claudeJSON); err != nil {
		return fmt.Errorf("chown claude config: %v: %s", err, out)
	}
	return nil
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
