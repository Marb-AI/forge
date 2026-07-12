package cli

import (
	"strings"
	"testing"
)

func TestDockerPruneIsConservative(t *testing.T) {
	script := buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root", true, false, false, true)

	// The whole point: never -a. `docker image prune -a` deletes tagged images no
	// container is running — i.e. every idle workspace's images, nightly.
	if strings.Contains(script, "image prune -a") || strings.Contains(script, "image prune -af") {
		t.Error("image prune must NOT use -a: it would delete idle workspaces' images")
	}
	// Volumes hold data. They must never be pruned.
	if strings.Contains(script, "volume prune") {
		t.Fatal("volumes must never be pruned — that is where data lives")
	}
	// Nor containers: worth ~nothing next to the cache, and pruning one drops its
	// writable layer, so a stack stopped overnight would need `up`, not `start`.
	if strings.Contains(script, "container prune") {
		t.Error("stopped containers must not be pruned")
	}
	// Nothing built today may be touched.
	for _, want := range []string{
		"docker image prune -f --filter until=24h",
		"docker builder prune -f --filter until=24h",
		"OnCalendar=*-*-* 03:00:00",
		"Persistent=true",
		"systemctl enable --now forge-docker-prune.timer",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prepare script is missing %q", want)
		}
	}
}

func TestDockerPruneCanBeDeclined(t *testing.T) {
	script := buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root", true, false, false, false)
	if strings.Contains(script, "forge-docker-prune") {
		t.Error("--no-docker-prune must leave the timer out entirely")
	}
}
