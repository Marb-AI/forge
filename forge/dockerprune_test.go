package forge

import (
	"strings"
	"testing"
)

// prepareScriptWith builds the provisioning script with the clean-up on or off,
// at its conservative default (no opt-in tier).
func prepareScriptWith(dockerPrune bool) string {
	return buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root",
		true, false, false, dockerPrune, false, false)
}

// prepareScriptAggressive builds it with the opt-in `--docker-prune-images` tier on.
func prepareScriptAggressive() string {
	return buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root",
		true, false, false, true, true, false)
}

// prepareScriptAllVolumes builds it with the opt-in `--docker-prune-volumes` tier on.
func prepareScriptAllVolumes() string {
	return buildPrepareScript("apt-get", "iproute2", "openssh-client", "amd64", 22, "root",
		true, false, false, true, false, true)
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

// The invariant the conservative default rests on: `docker image prune -a` deletes
// every tagged image no container holds — which, across several workspaces, can mean
// deleting the images of a project that is merely down tonight. By default it must
// never appear, in any spelling. (The opt-in `--docker-prune-images` tier adds it
// deliberately; that path is covered separately below.)
func TestImagePruneNeverAsksForAllByDefault(t *testing.T) {
	for _, cmd := range dockerPruneCmds(prepareScriptWith(true)) {
		if strings.Contains(cmd, "image prune") && asksForAll(cmd) {
			t.Errorf("default image prune must never use -a/--all: %s", cmd)
		}
	}
}

// The opt-in tier, and only it, adds the guarded `-a` sweep: it reaps the superseded
// tagged builds a rebuild-to-a-new-tag leaves behind, filtered to the grace window so
// every recent rebuild — and so the newest build of any repo — is safe.
func TestImagePruneOptInAddsGuardedAllSweep(t *testing.T) {
	var sweep string
	for _, cmd := range dockerPruneCmds(prepareScriptAggressive()) {
		if strings.Contains(cmd, "image prune") && asksForAll(cmd) {
			sweep = cmd
		}
	}
	if sweep == "" {
		t.Fatal("--docker-prune-images must add an `image prune -a` sweep")
	}
	if !strings.Contains(sweep, "until="+pruneImagesGrace) {
		t.Errorf("the -a sweep must be filtered to the grace window %s: %s", pruneImagesGrace, sweep)
	}
}

// Even aggressive, the sweep never touches the things that lose data or force a
// rebuild of a merely-stopped stack. The volume pass is exempt — it has its own
// safety line (anonymous-only by default), asserted separately below.
func TestImagePruneOptInStaysClearOfDataAndContainers(t *testing.T) {
	for _, cmd := range dockerPruneCmds(prepareScriptAggressive()) {
		for _, forbidden := range []string{"container prune", "system prune"} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("even the opt-in sweep must never run %q: %s", forbidden, cmd)
			}
		}
	}
}

// The safety line the volume pass rests on, and the reason "never volumes" could
// be relaxed at all: without -a, `docker volume prune` removes ONLY volumes
// nobody named. A compose stack's data is always in a NAMED volume, so it cannot
// be in scope. With -a it is — which is what makes that a separate, opt-in tier
// rather than the default.
func TestVolumePruneNeverAsksForAllByDefault(t *testing.T) {
	for _, cmd := range dockerPruneCmds(prepareScriptWith(true)) {
		if strings.Contains(cmd, "volume prune") && asksForAll(cmd) {
			t.Errorf("the default volume pass must never use -a/--all — that takes named volumes, i.e. a `compose down`-ed stack's data: %s", cmd)
		}
	}
}

// And the opt-in tier, and only it, widens to every unused volume.
func TestVolumePruneOptInTakesNamedVolumes(t *testing.T) {
	var sweep string
	for _, cmd := range dockerPruneCmds(prepareScriptAllVolumes()) {
		if strings.Contains(cmd, "volume prune") && asksForAll(cmd) {
			sweep = cmd
		}
	}
	if sweep == "" {
		t.Fatal("--docker-prune-volumes must widen the volume pass to -a")
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
		// Containers are worth ~nothing next to the cache, and pruning one drops its
		// writable layer, so a stack stopped overnight would have to be re-created.
		// `system prune` is all of these at once with no way to constrain it.
		for _, forbidden := range []string{"container prune", "system prune"} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("the clean-up must never run %q: %s", forbidden, cmd)
			}
		}
	}

	// And it prunes exactly the three things it is meant to — no more. Anything
	// added here has to be a decision, not a drift.
	want := []string{
		"docker image prune -f --filter until=24h || failed=1",
		"docker builder prune -af || failed=1",
		"docker volume prune -f || failed=1",
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

// The build-cache pass must NOT carry an `until` filter, which is the opposite of
// what it looked like it wanted.
//
// BuildKit's `until` filters on LAST USED, not on age, so `until=24h` means
// "cache nothing has touched in a day" — and on a build server almost every
// record is touched daily, so the filter spares nearly all of it. Measured
// against a cache whose records were all CREATED >30s earlier but LAST USED
// <1s earlier: `--filter until=30s` reclaimed 0 B and every record survived,
// while `--filter until=1s` reclaimed the lot. Creation-time semantics would
// have emptied it in the first call.
//
// A host running this nightly still reached 95 GB of build cache and a 98% full
// disk; an unfiltered `-af` on that cache reclaimed 93 GB. So this gate is the
// filter's tombstone: re-adding one silently restores a clean-up that reports
// success and reclaims nothing.
func TestBuildCachePruneIsUnfilteredAndComplete(t *testing.T) {
	var builder string
	for _, cmd := range dockerPruneCmds(prepareScriptWith(true)) {
		if strings.Contains(cmd, "builder prune") {
			builder = cmd
		}
	}
	if builder == "" {
		t.Fatal("the clean-up must prune the build cache — it is where the growth is")
	}
	if strings.Contains(builder, "until=") {
		t.Errorf("the build-cache pass must not use an `until` filter (it measures LAST USED, so it spares an active host's whole cache): %s", builder)
	}
	if !asksForAll(builder) {
		t.Errorf("the build-cache pass must use -a, or it cannot reach records an image still shares: %s", builder)
	}
}

// A clean-up whose success path is indistinguishable from its no-op path is not
// a safeguard, it is a report. Every pass used to end in `|| true` and the unit
// reported success whether it had reclaimed 168 GB or nothing at all — which is
// how a clean-up that had stopped working went unnoticed while the disk filled.
//
// So: no pass may swallow its own failure, and the script has to end by checking
// what actually happened to the disk.
func TestDockerPruneReportsFailure(t *testing.T) {
	script := prepareScriptWith(true)

	for _, cmd := range dockerPruneCmds(script) {
		if strings.Contains(cmd, "|| true") {
			t.Errorf("a prune pass must not swallow its failure with `|| true`: %s", cmd)
		}
	}
	// It measures the filesystem rather than trusting the commands, and turns
	// "still nearly full" into a non-zero exit.
	for _, want := range []string{
		"FULL_PCT=" + pruneFullPct,
		`if [ "$after_pct" -ge "$FULL_PCT" ]; then`,
		`if [ "$failed" -ne 0 ]; then`,
		"exit 1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the clean-up must verify its own result; missing %q", want)
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
