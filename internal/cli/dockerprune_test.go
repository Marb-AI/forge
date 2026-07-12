package cli

import (
	"strings"
	"testing"
)

// prepareScriptWith builds the provisioning script with the clean-up on or off.
func prepareScriptWith(dockerPrune bool) string {
	return buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root",
		true, false, false, dockerPrune)
}

// dockerPruneCmds returns the `docker … prune …` commands the script will actually
// run, so the assertions below examine the real commands instead of pattern-matching
// the whole script — a substring check happily misses `prune -f -a`.
func dockerPruneCmds(script string) []string {
	var cmds []string
	for _, line := range strings.Split(script, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "docker ") && strings.Contains(l, "prune") {
			cmds = append(cmds, l)
		}
	}
	return cmds
}

// asksForAll reports whether a docker command asks for -a/--all, in every form it
// can take: separate (-a), long (--all), or bundled into a short-flag group (-af,
// -fa). Position doesn't matter.
func asksForAll(cmd string) bool {
	for _, f := range strings.Fields(cmd) {
		switch {
		case f == "--all", f == "-a":
			return true
		case len(f) > 1 && f[0] == '-' && f[1] != '-' && strings.ContainsRune(f[1:], 'a'):
			return true // bundled shorts
		}
	}
	return false
}

// The invariant the whole feature rests on: `docker image prune -a` deletes every
// tagged image no container happens to be running — which, across several
// workspaces, means quietly deleting the images of every project that isn't up
// tonight. It must never appear, in any spelling.
func TestImagePruneNeverAsksForAll(t *testing.T) {
	for _, cmd := range dockerPruneCmds(prepareScriptWith(true)) {
		if strings.Contains(cmd, "image prune") && asksForAll(cmd) {
			t.Errorf("image prune must never use -a/--all: %s", cmd)
		}
	}
}

// asksForAll has to be right, or the test above is theatre.
func TestAsksForAllCatchesEverySpelling(t *testing.T) {
	all := []string{
		"docker image prune -a",
		"docker image prune --all",
		"docker image prune -af --filter until=24h",
		"docker image prune -fa --filter until=24h",
		"docker image prune -f -a --filter until=24h", // the one a substring check misses
		"docker image prune --filter until=24h --all",
	}
	for _, c := range all {
		if !asksForAll(c) {
			t.Errorf("asksForAll(%q) = false, want true", c)
		}
	}
	none := []string{
		"docker image prune -f --filter until=24h",
		"docker builder prune -f --filter until=24h",
		"docker system df",
	}
	for _, c := range none {
		if asksForAll(c) {
			t.Errorf("asksForAll(%q) = true, want false", c)
		}
	}
}

// What the timer prunes — and, more importantly, what it must never touch.
func TestDockerPruneIsConservative(t *testing.T) {
	cmds := dockerPruneCmds(prepareScriptWith(true))

	for _, cmd := range cmds {
		// Volumes hold data: `docker system df` calls them "100% reclaimable" whenever
		// no container is holding them, which is how the copy-pasted prune cron job
		// ends up eating databases. Containers are worth ~nothing next to the cache,
		// and pruning one drops its writable layer, so a stack stopped overnight would
		// have to be re-created.
		for _, forbidden := range []string{"volume prune", "container prune", "system prune"} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("the clean-up must never run %q: %s", forbidden, cmd)
			}
		}
		// Nothing built today may be swept up, whatever the command turns out to be.
		if !strings.Contains(cmd, "until=24h") {
			t.Errorf("every prune must be filtered to 24h: %s", cmd)
		}
	}

	// And it prunes exactly the two things it is meant to — no more. Anything added
	// here has to be a decision, not a drift.
	want := []string{
		"docker image prune -f --filter until=24h || true",
		"docker builder prune -f --filter until=24h || true",
	}
	if len(cmds) != len(want) {
		t.Fatalf("expected exactly %d prune commands, got %d: %v", len(want), len(cmds), cmds)
	}
	for i, w := range want {
		if cmds[i] != w {
			t.Errorf("prune command %d = %q, want %q", i, cmds[i], w)
		}
	}
}

// The unit must not Require docker: that fails the timer outright on a host where
// Docker was removed — a permanently red unit — and starts Docker on one where it
// was stopped on purpose. The script already exits cleanly when Docker is absent,
// which is the behaviour we want.
func TestPruneUnitDoesNotRequireDocker(t *testing.T) {
	script := prepareScriptWith(true)
	if strings.Contains(script, "Requires=docker.service") {
		t.Error("the unit must not Requires=docker.service: it would fail forever without Docker, and start it when stopped")
	}
	if !strings.Contains(script, "After=docker.service") {
		t.Error("the unit should still be ordered After=docker.service")
	}
}

func TestPruneTimerIsScheduledAndPersistent(t *testing.T) {
	script := prepareScriptWith(true)
	for _, want := range []string{
		"OnCalendar=*-*-* 03:00:00",
		"Persistent=true", // a host that was off at 03:00 still runs it on return
		"RandomizedDelaySec",
		"systemctl enable --now forge-docker-prune.timer",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prepare script is missing %q", want)
		}
	}
}

func TestDockerPruneCanBeDeclined(t *testing.T) {
	if strings.Contains(prepareScriptWith(false), "forge-docker-prune") {
		t.Error("--no-docker-prune must leave the timer out entirely")
	}
}
