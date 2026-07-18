package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
)

func TestNameValidation(t *testing.T) {
	ok := []string{"crm", "api2", "shop-1", "a_b"}
	for _, n := range ok {
		if !nameRe.MatchString(n) {
			t.Errorf("expected %q valid", n)
		}
	}
	bad := []string{"", "1crm", "CRM", "has space", "with/slash", "-lead", strings.Repeat("x", 40)}
	for _, n := range bad {
		if nameRe.MatchString(n) {
			t.Errorf("expected %q invalid", n)
		}
	}
}

func TestWriteEnvFile(t *testing.T) {
	home := t.TempDir()
	if err := writeEnvFile(home, "crm"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, envRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "COMPOSE_PROJECT_NAME=crm") {
		t.Errorf("env content = %q", data)
	}
}

func TestSeedBashrcGuardAndSource(t *testing.T) {
	home := t.TempDir()
	if err := seedBashrc(home, "crm"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".bashrc"))
	s := string(data)
	if !strings.Contains(s, "claude()") {
		t.Error("missing claude guard function")
	}
	if !strings.Contains(s, ".forge/env") {
		t.Error("bashrc should source the env file")
	}
	if !strings.Contains(s, "forge workspace crm claude") {
		t.Error("guard should name the workspace")
	}
}

func TestSeedSSHAuthorizedKeys(t *testing.T) {
	home := t.TempDir()
	if err := seedSSH(home, "crm", []byte("ssh-ed25519 AAAA")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "ssh-ed25519 AAAA") || !strings.HasSuffix(string(data), "\n") {
		t.Errorf("authorized_keys = %q", data)
	}
}

func TestWriteMetadata(t *testing.T) {
	home := t.TempDir()
	if err := writeMetadata(home, "crm"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "workspace.json"))
	for _, want := range []string{`"name": "crm"`, `"tmux_session": "claude"`, "created_at"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("metadata missing %q in %s", want, data)
		}
	}
}

func TestSeedTmuxConf(t *testing.T) {
	home := t.TempDir()
	if err := seedTmuxConf(home); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".tmux.conf"))
	if !strings.Contains(string(data), "status off") {
		t.Errorf("tmux.conf = %q", data)
	}
}

func TestSeedGitconfig(t *testing.T) {
	home := t.TempDir()
	if err := seedGitconfig(home); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if !strings.Contains(string(data), "defaultBranch = main") {
		t.Errorf("gitconfig = %q", data)
	}
}

// TestSeedGitKey covers the two things the copy must get right: the private key
// lands at git's default path with 0600 (ssh refuses a world-readable key), and a
// host with no identity is a warning, not a failed workspace create.
func TestSeedGitKey(t *testing.T) {
	t.Run("copies key and known_hosts", func(t *testing.T) {
		keyDir, home := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string]string{
			"id_ed25519":     "PRIVATE",
			"id_ed25519.pub": "ssh-ed25519 AAAA forge@host",
			"known_hosts":    "github.com ssh-ed25519 AAAA",
		} {
			if err := os.WriteFile(filepath.Join(keyDir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		if err := seedGitKey(home, keyDir); err != nil {
			t.Fatalf("seedGitKey: %v", err)
		}

		priv := filepath.Join(home, ".ssh", "id_ed25519")
		got, err := os.ReadFile(priv)
		if err != nil || string(got) != "PRIVATE" {
			t.Fatalf("private key not copied: %q, %v", got, err)
		}
		fi, err := os.Stat(priv)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("private key mode = %o, want 600 (ssh rejects looser)", perm)
		}
		for _, f := range []string{"id_ed25519.pub", "known_hosts"} {
			if _, err := os.Stat(filepath.Join(home, ".ssh", f)); err != nil {
				t.Errorf("%s not copied: %v", f, err)
			}
		}
	})

	t.Run("missing host key is not an error", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := seedGitKey(home, filepath.Join(t.TempDir(), "absent")); err != nil {
			t.Errorf("seedGitKey on a host with no identity: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".ssh", "id_ed25519")); !os.IsNotExist(err) {
			t.Error("expected no key to be written")
		}
	})
}

// TestWriteClaudeConfig pins both prompts a workspace must never show: the folder
// trust dialog and the per-tool permission prompt. Either one stalls a session
// that nobody is watching, which is the whole failure Forge exists to avoid.
func TestWriteClaudeConfig(t *testing.T) {
	home := t.TempDir()
	if err := writeClaudeConfig(home); err != nil {
		t.Fatalf("writeClaudeConfig: %v", err)
	}

	read := func(p string) map[string]any {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		return m
	}

	cj := read(filepath.Join(home, ".claude.json"))
	projects, _ := cj["projects"].(map[string]any)
	proj, _ := projects[home].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true {
		t.Errorf("trust dialog not pre-accepted for %s: %v", home, cj)
	}

	st := read(filepath.Join(home, ".claude", "settings.json"))
	perms, _ := st["permissions"].(map[string]any)
	if got := perms["defaultMode"]; got != "bypassPermissions" {
		t.Errorf("permissions.defaultMode = %v, want bypassPermissions", got)
	}
}

// TestWriteClaudeConfigPreservesExisting: the workspace user may have edited
// these files (logged in, added an MCP server). Re-seeding must merge, not clobber.
func TestWriteClaudeConfigPreservesExisting(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte(`{"model":"opus","permissions":{"allow":["Bash(ls:*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeClaudeConfig(home); err != nil {
		t.Fatalf("writeClaudeConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "opus" {
		t.Errorf("unrelated key dropped: %v", m)
	}
	perms, _ := m["permissions"].(map[string]any)
	if perms["defaultMode"] != "bypassPermissions" {
		t.Errorf("defaultMode not set: %v", perms)
	}
	if _, ok := perms["allow"]; !ok {
		t.Errorf("sibling key under permissions dropped: %v", perms)
	}
}

// The activity hooks are what make the UI's idle indicator possible: on Stop
// (Claude finished) the workspace records "idle", which the browser turns into a
// "waiting for you" mark on the tab. If the hook isn't written, or writes the
// wrong file, the indicator silently never lights up.
func TestWriteClaudeConfigInstallsActivityHooks(t *testing.T) {
	home := t.TempDir()
	if err := writeClaudeConfig(home); err != nil {
		t.Fatalf("writeClaudeConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	// hooks.Stop[0].hooks[0].command must fire and write "idle" to the activity file.
	hooks, _ := m["hooks"].(map[string]any)
	for _, ev := range []string{"UserPromptSubmit", "Stop", "Notification"} {
		if _, ok := hooks[ev]; !ok {
			t.Fatalf("hooks.%s missing: %v", ev, hooks)
		}
	}
	cmd := firstHookCommand(t, hooks, "Stop")
	if !strings.Contains(cmd, "forge-activity") || !strings.Contains(cmd, agentproto.ActivityIdle) {
		t.Errorf("Stop hook doesn't record idle to the activity file: %q", cmd)
	}
}

func firstHookCommand(t *testing.T, hooks map[string]any, event string) string {
	t.Helper()
	matchers, ok := hooks[event].([]any)
	if !ok || len(matchers) == 0 {
		t.Fatalf("hooks.%s not a non-empty array: %v", event, hooks[event])
	}
	first, _ := matchers[0].(map[string]any)
	inner, ok := first["hooks"].([]any)
	if !ok || len(inner) == 0 {
		t.Fatalf("hooks.%s[0].hooks not a non-empty array: %v", event, first)
	}
	h0, _ := inner[0].(map[string]any)
	cmd, _ := h0["command"].(string)
	return cmd
}

// ensureActivityHooks uses the bare string "forge-activity" in settings.json as
// its "already installed" marker. Confirm a seeded config carries it and a bare
// one doesn't — if the marker were wrong, self-healing would rewrite the config
// (and chown it) on every single poll.
func TestActivityHooksMarker(t *testing.T) {
	home := t.TempDir()
	if err := writeClaudeConfig(home); err != nil {
		t.Fatal(err)
	}
	seeded, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(seeded), "forge-activity") {
		t.Errorf("seeded settings.json lacks the marker: %s", seeded)
	}
	if strings.Contains(`{"permissions":{"defaultMode":"bypassPermissions"}}`, "forge-activity") {
		t.Error("a config without our hooks must not match the marker")
	}
}

func TestParseActivity(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		state string
		ts    int64
	}{
		{"idle 1784386328", true, "idle", 1784386328},
		{"busy 42\n", true, "busy", 42},
		{"waiting", true, "waiting", 0}, // no timestamp yet: state still usable
		{"", false, "", 0},
		{"   \n", false, "", 0},
	}
	for _, c := range cases {
		a, ok := parseActivity([]byte(c.in))
		if ok != c.ok || a.State != c.state || a.TS != c.ts {
			t.Errorf("parseActivity(%q) = (%+v, %v), want (state=%q ts=%d, ok=%v)",
				c.in, a, ok, c.state, c.ts, c.ok)
		}
	}
}

// TestTmuxConfEnablesCopy pins the settings a remote session needs to be usable
// from a laptop: mouse on (so tmux owns the drag and can copy at all) and
// set-clipboard on (so the yank travels home over OSC 52). Losing either one
// silently makes text in the session unselectable-but-uncopyable.
func TestTmuxConfEnablesCopy(t *testing.T) {
	for _, want := range []string{
		"set -g status off",
		"set -g mouse on",
		"set -g set-clipboard on",
		"copy-selection-and-cancel",
	} {
		if !strings.Contains(tmuxConf, want) {
			t.Errorf("tmux conf is missing %q:\n%s", want, tmuxConf)
		}
	}
	// The binding must exist for whichever key table the workspace ends up in;
	// tmux picks copy-mode-vi when the user's EDITOR looks like vi.
	for _, table := range []string{"copy-mode ", "copy-mode-vi "} {
		if !strings.Contains(tmuxConf, "bind -T "+table) {
			t.Errorf("no MouseDragEnd1Pane binding for key table %q", strings.TrimSpace(table))
		}
	}
}

func TestSeedTmuxConfWritesFile(t *testing.T) {
	home := t.TempDir()
	if err := seedTmuxConf(home); err != nil {
		t.Fatalf("seedTmuxConf: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != tmuxConf {
		t.Errorf("written conf differs from tmuxConf")
	}
}

// TestSeedGhAuth: the token file must land where gh looks for it, readable only
// by the workspace user, and a host that was never logged in must not fail the
// create — it just has no gh credential yet.
func TestSeedGhAuth(t *testing.T) {
	t.Run("copies hosts.yml 0600", func(t *testing.T) {
		ghDir, home := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte("github.com:\n  oauth_token: gho_secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := seedGhAuth(home, ghDir); err != nil {
			t.Fatalf("seedGhAuth: %v", err)
		}
		dst := filepath.Join(home, ".config", "gh", "hosts.yml")
		got, err := os.ReadFile(dst)
		if err != nil || !strings.Contains(string(got), "gho_secret") {
			t.Fatalf("token not copied: %q, %v", got, err)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("hosts.yml mode = %o, want 600 — it holds an OAuth token", perm)
		}
	})

	t.Run("no host login is not an error", func(t *testing.T) {
		home := t.TempDir()
		if err := seedGhAuth(home, filepath.Join(t.TempDir(), "absent")); err != nil {
			t.Errorf("seedGhAuth on a host with no gh login: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "gh", "hosts.yml")); !os.IsNotExist(err) {
			t.Error("expected no hosts.yml to be written")
		}
	})
}

// fakeProc builds a procfs fixture: pid -> real UID, plus the noise a real /proc
// carries — non-process entries (a named directory, a named file) and pid 7777,
// whose status cannot be read, standing in for a process that exits mid-scan.
// None of the noise may show up as a pid to kill.
func fakeProc(t *testing.T, owners map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, uid := range owners {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("Name:\tsshd\nUid:\t%[1]s\t%[1]s\t%[1]s\t%[1]s\nGid:\t%[1]s\n", uid)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The noise a real /proc carries: named entries that are not processes (a
	// directory and a file), and a process that vanishes mid-scan — modelled here
	// as a pid whose status is unreadable, which is what a scan actually races.
	if err := os.MkdirAll(filepath.Join(root, "irq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cpuinfo"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "7777"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUserPIDsFindsOnlyTheUsersProcesses(t *testing.T) {
	root := fakeProc(t, map[int]string{1: "0", 4242: "1001", 4243: "1001", 9000: "1002"})

	pids, err := userPIDs(root, "1001")
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(pids)
	if !slices.Equal(pids, []int{4242, 4243}) {
		t.Errorf("pids = %v, want [4242 4243]", pids)
	}

	none, err := userPIDs(root, "1003")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("pids for an unused uid = %v, want none", none)
	}
}

// The agent must never signal itself out of existence mid-delete, even if it were
// somehow running as the workspace user.
func TestUserPIDsSkipsSelf(t *testing.T) {
	self := os.Getpid()
	root := fakeProc(t, map[int]string{self: "1001", 4242: "1001"})

	pids, err := userPIDs(root, "1001")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{4242}) {
		t.Errorf("pids = %v, want [4242] (self excluded)", pids)
	}
}

func TestProcUIDReadsTheRealUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	// Real uid 1001, but effective 0 — we want the first field, not any other.
	if err := os.WriteFile(path, []byte("Name:\tbash\nUid:\t1001\t0\t0\t0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uid, err := procUID(path)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "1001" {
		t.Errorf("uid = %q, want 1001", uid)
	}
	if _, err := procUID(filepath.Join(dir, "gone", "status")); err == nil {
		t.Error("a vanished process should be an error, not a silent match")
	}
}

// A user who is simply not there is nothing to reap. Anything else that goes
// wrong in the lookup must be reported: reaping is what makes the delete work, so
// skipping it quietly would hand us back the userdel exit-8 failure it prevents.
func TestReapUserIgnoresOnlyAnUnknownUser(t *testing.T) {
	if err := reapUser("no-such-workspace-user"); err != nil {
		t.Errorf("unknown user should be a no-op, got %v", err)
	}
}
