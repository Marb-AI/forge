package cli

import (
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
		{"apt-root-all", buildPrepareScript("apt-get", "iproute2", 22, "root", true, true, true)},
		{"dnf-nonroot", buildPrepareScript("dnf", "iproute", 2222, "deploy", false, true, true)},
		{"yum-minimal", buildPrepareScript("yum", "iproute", 22, "root", true, false, false)},
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
		})
	}
}
