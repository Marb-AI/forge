package agent

import (
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
