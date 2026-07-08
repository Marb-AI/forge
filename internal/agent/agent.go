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
	if err := seedBashrc(home, *name); err != nil {
		return emitError("bashrc: %v", err)
	}
	if err := seedGitconfig(home); err != nil {
		return emitError("gitconfig: %v", err)
	}
	if err := writeMetadata(home, *name); err != nil {
		return emitError("metadata: %v", err)
	}
	// Own everything by the workspace user.
	if out, err := run("chown", "-R", *name+":"+*name, home); err != nil {
		return emitError("chown: %v: %s", err, out)
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

// forgeBashrcBlock is appended to the workspace user's .bashrc. %[1]s is the
// workspace name. It (a) makes `docker compose`'s project name unique per
// workspace automatically — so it never collides across workspaces or clones,
// and it enforces the project==workspace convention the port scan relies on —
// and (b) shadows the `claude` binary for interactive shells so a stray launch
// (which would die on disconnect) is redirected to the managed flow.
const forgeBashrcBlock = `
# --- forge: workspace environment ---
export COMPOSE_PROJECT_NAME=%[1]s

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
