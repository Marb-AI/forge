package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
