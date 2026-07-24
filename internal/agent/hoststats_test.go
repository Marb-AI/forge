package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A real aggregate line, plus two cores. The last two fields (guest, guest_nice)
// are already counted inside user and nice — summing them again would inflate the
// denominator and quietly understate every reading.
const procStatSample = `cpu  1000 100 300 8000 200 0 50 10 20 5
cpu0 500 50 150 4000 100 0 25 5 10 2
cpu1 500 50 150 4000 100 0 25 5 10 3
intr 12345
ctxt 67890
btime 1700000000
`

func TestParseCPUStat(t *testing.T) {
	idle, total, cores, ok := parseCPUStat([]byte(procStatSample))
	if !ok {
		t.Fatal("a normal /proc/stat should parse")
	}
	// idle + iowait: a core waiting on a disk is not doing work.
	if want := uint64(8000 + 200); idle != want {
		t.Errorf("idle = %d, want %d (idle + iowait)", idle, want)
	}
	// user..steal, and nothing after it.
	if want := uint64(1000 + 100 + 300 + 8000 + 200 + 0 + 50 + 10); total != want {
		t.Errorf("total = %d, want %d (user..steal, guest excluded)", total, want)
	}
	if cores != 2 {
		t.Errorf("cores = %d, want 2", cores)
	}
}

func TestParseCPUStatRejectsGarbage(t *testing.T) {
	if _, _, _, ok := parseCPUStat([]byte("not a proc file at all\n")); ok {
		t.Error("a file with no cpu line must not report a measurement")
	}
	if _, _, _, ok := parseCPUStat([]byte("cpu  x y z\n")); ok {
		t.Error("non-numeric fields must not be read as jiffies")
	}
}

// The percentage has to come from a DELTA. /proc/stat holds counters since boot,
// so a single read reports the average since the machine was switched on — a
// server up for a month and on fire right now would report a calm few percent.
func TestCPUUsageMeasuresOverTheWindow(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "stat")
	// First sample: all idle so far.
	writeFile(t, stat, "cpu  0 0 0 1000 0 0 0 0\ncpu0 0 0 0 1000 0 0 0 0\n")

	// Second sample lands while cpuUsage sleeps: 300 busy jiffies against 100 idle
	// over the interval, i.e. 75%.
	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		// Not t.Fatalf: this is not the test's own goroutine.
		done <- os.WriteFile(stat, []byte("cpu  200 0 100 1100 0 0 0 0\ncpu0 200 0 100 1100 0 0 0 0\n"), 0o644)
	}()

	withProcRoot(t, dir)
	pct, cores, ok := cpuUsage(200 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if !ok {
		t.Fatal("cpuUsage should have measured")
	}
	if cores != 1 {
		t.Errorf("cores = %d, want 1", cores)
	}
	if pct < 74.9 || pct > 75.1 {
		t.Errorf("cpu = %.2f%%, want 75%% (300 busy of 400 jiffies over the window)", pct)
	}
}

// Two identical samples mean the window closed inside a single tick. The cores
// are still real, but there is no percentage to report — and inventing one from
// the since-boot totals is exactly the bug the two samples exist to avoid.
func TestCPUUsageOnAnUnchangedCounter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "stat"), procStatSample)
	withProcRoot(t, dir)

	pct, cores, ok := cpuUsage(time.Millisecond)
	if !ok || cores != 2 {
		t.Fatalf("cpuUsage = (%v, %d, %v), want the cores through", pct, cores, ok)
	}
	if pct != 0 {
		t.Errorf("cpu = %.2f%%, want 0 — nothing moved between the samples", pct)
	}
}

func TestCPUUsageWithoutProcStat(t *testing.T) {
	withProcRoot(t, t.TempDir())
	if _, _, ok := cpuUsage(time.Millisecond); ok {
		t.Error("no /proc/stat must report no measurement, not a zero one")
	}
}

// Used is total minus MemAvailable, never minus MemFree: Linux spends every spare
// byte on page cache, so a healthy server has almost no free memory and MemFree
// would report it as 97% used.
func TestParseMemInfoUsesAvailable(t *testing.T) {
	data := []byte(`MemTotal:       16305892 kB
MemFree:          204864 kB
MemAvailable:   12241236 kB
Buffers:          105232 kB
Cached:          9432104 kB
`)
	total, used, ok := parseMemInfo(data)
	if !ok {
		t.Fatal("a normal /proc/meminfo should parse")
	}
	if want := uint64(16305892) * 1024; total != want {
		t.Errorf("total = %d, want %d bytes", total, want)
	}
	if want := (uint64(16305892) - 12241236) * 1024; used != want {
		t.Errorf("used = %d, want %d (total - MemAvailable)", used, want)
	}
	// The trap: MemFree would have called this machine nearly full.
	if float64(used)/float64(total) > 0.5 {
		t.Errorf("used is %.0f%% of a machine with 12 GB available", float64(used)/float64(total)*100)
	}
}

// Kernels before 3.14 have no MemAvailable; the fallback keeps them measurable.
func TestParseMemInfoFallsBackToFreePlusCache(t *testing.T) {
	data := []byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 250 kB\n")
	total, used, ok := parseMemInfo(data)
	if !ok || total != 1000*1024 {
		t.Fatalf("total = %d, ok = %v", total, ok)
	}
	if want := uint64(1000-400) * 1024; used != want {
		t.Errorf("used = %d, want %d (total - free - buffers - cached)", used, want)
	}
}

func TestParseMemInfoRejectsGarbage(t *testing.T) {
	if _, _, ok := parseMemInfo([]byte("hello\n")); ok {
		t.Error("a file with no MemTotal must not report a measurement")
	}
}

// The disk figures come from the kernel, so the test asks it about a directory
// that certainly exists rather than checking arithmetic against a fixture.
func TestDiskUsageOnARealFilesystem(t *testing.T) {
	total, used, ok := diskUsage(t.TempDir())
	if !ok {
		t.Fatal("statfs on a temp dir should work")
	}
	if total == 0 || used > total {
		t.Errorf("used %d of %d bytes: nonsense", used, total)
	}
}

func TestDiskUsageOnAMissingPath(t *testing.T) {
	if _, _, ok := diskUsage(filepath.Join(t.TempDir(), "nope")); ok {
		t.Error("a path that isn't there must report no measurement")
	}
}

// diskPath measures where the workspaces live — the disk that actually fills up —
// and falls back to the root on a host that has none yet.
func TestDiskPathPrefersTheWorkspaceFilesystem(t *testing.T) {
	dir := t.TempDir()
	old := baseDir
	baseDir = dir
	t.Cleanup(func() { baseDir = old })
	if got := diskPath(); got != dir {
		t.Errorf("diskPath() = %q, want the workspace directory %q", got, dir)
	}

	baseDir = filepath.Join(dir, "not-created-yet")
	if got := diskPath(); got != "/" {
		t.Errorf("diskPath() = %q, want \"/\" when there are no workspaces", got)
	}
}

func TestParseUptime(t *testing.T) {
	if got := parseUptime([]byte("372457.51 2872381.90\n")); got != 372457 {
		t.Errorf("parseUptime = %d, want 372457", got)
	}
	for _, junk := range []string{"", "nonsense\n", "-5 1\n"} {
		if got := parseUptime([]byte(junk)); got != 0 {
			t.Errorf("parseUptime(%q) = %d, want 0", junk, got)
		}
	}
}

func withProcRoot(t *testing.T, dir string) {
	t.Helper()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
