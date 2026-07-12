package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestPrepareScriptSyntax validates the generated provisioning script parses as
// valid bash (bash -n = syntax check, no execution) across distro/flag combos.
func TestPrepareScriptSyntax(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"apt-root-all", buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root", true, true, true, true)},
		{"dnf-nonroot", buildPrepareScript("dnf", "iproute", "openssh-clients", "arm64", 2222, "deploy", false, true, true, true)},
		{"yum-minimal", buildPrepareScript("yum", "iproute", "openssh-clients", "amd64", 22, "root", true, false, false, false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(c.script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n rejected script: %v\n%s", err, out)
			}
			// sudoers rule only when non-root.
			hasSudoers := strings.Contains(c.script, "sudoers.d/forge")
			if c.name == "dnf-nonroot" && !hasSudoers {
				t.Error("expected sudoers setup for non-root user")
			}
			if c.name == "apt-root-all" && hasSudoers {
				t.Error("did not expect sudoers setup for root")
			}
			// Every distro gets gh, make, and a git identity.
			if !strings.Contains(c.script, "gh") {
				t.Error("expected gh install")
			}
			if !strings.Contains(c.script, "ensure make make") {
				t.Error("expected make install")
			}
			if !strings.Contains(c.script, hostKeyPath) {
				t.Errorf("expected git identity at %s", hostKeyPath)
			}
		})
	}
}

// TestPrepareKeyGenIsGuarded pins the property that makes prepare safe to re-run:
// ssh-keygen must never run unconditionally. Regenerating the key would silently
// break every repo and host the old public key is already registered on, and the
// failure would surface much later as a permission-denied from GitHub.
func TestPrepareKeyGenIsGuarded(t *testing.T) {
	script := buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root", true, true, true, true)

	// The generating call must sit inside the `[ -f "$KEY" ]` else-branch.
	idx := strings.Index(script, "ssh-keygen -q -t ed25519")
	if idx < 0 {
		t.Fatal("no ed25519 keygen found in script")
	}
	guard := "if [ -f \"" + hostKeyPath + "\" ]; then"
	gi := strings.Index(script, guard)
	if gi < 0 || gi > idx {
		t.Errorf("keygen is not guarded by an existence check on %s", hostKeyPath)
	}

	// And the guard must actually work: run the section twice against a temp
	// key path and assert the key is untouched the second time.
	dir := t.TempDir()
	section := strings.NewReplacer(
		"__KEYDIR__", dir,
		"__KEY__", dir+"/id_ed25519",
		"__SSHCLIENT__", "openssh-client",
	).Replace(sshKeySection)
	// Drop the parts that need root or the network; keep the key logic.
	section = strings.ReplaceAll(section, `ensure ssh-keygen openssh-client "openssh-client"`, "")
	section = strings.ReplaceAll(section, "install -m 0755 -d "+dir, "")
	if i := strings.Index(section, "if [ ! -s "); i >= 0 {
		section = section[:i]
	}

	runSection := func() string {
		cmd := exec.Command("bash", "-s")
		cmd.Stdin = strings.NewReader("set -e\n" + section)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("key section failed: %v\n%s", err, out)
		}
		data, err := os.ReadFile(dir + "/id_ed25519")
		if err != nil {
			t.Fatalf("no key produced: %v", err)
		}
		return string(data)
	}

	first := runSection()
	if second := runSection(); first != second {
		t.Error("re-running prepare regenerated the host key; it must be kept")
	}
}
