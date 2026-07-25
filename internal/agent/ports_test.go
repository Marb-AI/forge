package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/agentproto"
)

func TestParseBlockFlags(t *testing.T) {
	// Neither flag is a client older than blocks, not an error.
	if b, err := parseBlockFlags(0, 0); err != nil || b != nil {
		t.Errorf("no flags = %v, %v; want nil, nil", b, err)
	}
	b, err := parseBlockFlags(16000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if b.Start != 16000 || b.Size != 100 || b.End() != 16099 {
		t.Errorf("block = %+v (end %d)", b, b.End())
	}
	// Half a block is a caller bug; guessing the other half would create a
	// workspace publishing ports nobody tunnels.
	for _, c := range [][2]int{{16000, 0}, {0, 100}, {-1, 100}, {16000, -1}} {
		if _, err := parseBlockFlags(c[0], c[1]); err == nil {
			t.Errorf("parseBlockFlags(%d, %d) = nil error; want one", c[0], c[1])
		}
	}
	if _, err := parseBlockFlags(65500, 100); err == nil {
		t.Error("a block running past 65535 should be refused")
	}
}

func TestWriteEnvFileCarriesBlock(t *testing.T) {
	home := t.TempDir()
	if err := writeEnvFile(home, "crm", &agentproto.PortBlock{Start: 16100, Size: 100}); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, filepath.Join(home, envRelPath)))
	for _, want := range []string{"FORGE_PORT_MIN=16100", "FORGE_PORT_MAX=16199"} {
		if !strings.Contains(got, want) {
			t.Errorf("env missing %q in:\n%s", want, got)
		}
	}
}

// A workspace with no block must not get FORGE_PORT_MIN=0: forge-ports keys off
// the variable's absence to tell "no block" from a block at port zero.
func TestWriteEnvFileWithoutBlock(t *testing.T) {
	home := t.TempDir()
	if err := writeEnvFile(home, "crm", nil); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(home, envRelPath))); strings.Contains(got, "FORGE_PORT") {
		t.Errorf("blockless env should mention no ports, got:\n%s", got)
	}
}

func TestPortsMemoryStatesTheRange(t *testing.T) {
	got := portsMemory(agentproto.PortBlock{Start: 16100, Size: 100})
	for _, want := range []string{"16100", "16199", "16100:3000", "forge-ports"} {
		if !strings.Contains(got, want) {
			t.Errorf("memory missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, portsMemoryStart) || !strings.HasSuffix(got, portsMemoryEnd) {
		t.Errorf("memory is not fenced:\n%s", got)
	}
	// The end of the block, not the start, must be the second number — an
	// off-by-one here hands the neighbour's ports out.
	if strings.Contains(got, "16100–16100") {
		t.Error("range collapsed to a single port")
	}
}

func TestSetPortsMemoryKeepsWhatItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, memoryRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "# My notes\n\nAlways run the tests.\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setPortsMemory(home, agentproto.PortBlock{Start: 16000, Size: 100}); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if !strings.Contains(got, "Always run the tests.") {
		t.Errorf("user's memory was eaten:\n%s", got)
	}
	if !strings.Contains(got, "16000–16099") {
		t.Errorf("section missing:\n%s", got)
	}

	// Re-running with a different block replaces the section rather than stacking
	// a second one — this runs on a sweep, so anything else grows without bound.
	if err := setPortsMemory(home, agentproto.PortBlock{Start: 16200, Size: 100}); err != nil {
		t.Fatal(err)
	}
	got = string(mustRead(t, path))
	if n := strings.Count(got, portsMemoryStart); n != 1 {
		t.Errorf("section appears %d times:\n%s", n, got)
	}
	if strings.Contains(got, "16000–16099") || !strings.Contains(got, "16200–16299") {
		t.Errorf("section not updated:\n%s", got)
	}
	if !strings.Contains(got, "Always run the tests.") {
		t.Errorf("user's memory lost on rewrite:\n%s", got)
	}
}

// Writing the same block twice must not touch the file at all: the sweep calls
// this repeatedly and a file that changes mtime every few seconds is noise.
func TestSetPortsMemoryIsIdempotent(t *testing.T) {
	home := t.TempDir()
	b := agentproto.PortBlock{Start: 16000, Size: 100}
	if err := setPortsMemory(home, b); err != nil {
		t.Fatal(err)
	}
	first := mustRead(t, filepath.Join(home, memoryRelPath))
	if err := setPortsMemory(home, b); err != nil {
		t.Fatal(err)
	}
	if second := mustRead(t, filepath.Join(home, memoryRelPath)); string(first) != string(second) {
		t.Errorf("second write differs:\n%s\n---\n%s", first, second)
	}
}

func TestReplaceSection(t *testing.T) {
	const s, e = "<!--a-->", "<!--b-->"
	cases := []struct{ name, doc, want string }{
		{"empty", "", "NEW\n"},
		{"appends", "hello", "hello\n\nNEW\n"},
		{"appends after newline", "hello\n", "hello\n\nNEW\n"},
		{"replaces", "top\n" + s + "old" + e + "\nbottom", "top\nNEW\nbottom"},
		// A start marker with no end is left alone: truncating everything after a
		// stray marker would eat whatever the user wrote below it.
		{"unterminated is left alone", "top\n" + s + "\nmine", "top\n" + s + "\nmine\n\nNEW\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := replaceSection(c.doc, s, e, "NEW"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestMetadataBlockRoundTrip(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	block := &agentproto.PortBlock{Start: 16300, Size: 100}
	if err := writeMetadata(home, "crm", block); err != nil {
		t.Fatal(err)
	}
	got := readMetadata("crm")
	if got.PortBlock == nil || *got.PortBlock != *block {
		t.Fatalf("block = %v, want %v", got.PortBlock, block)
	}
	if got.Name != "crm" || got.TmuxSession != agentproto.TmuxSession {
		t.Errorf("other fields lost: %+v", got)
	}
}

// A workspace with no metadata file, or an unreadable one, has no block — not an
// error. Every caller wants that answer rather than a failure.
func TestReadMetadataTolerant(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	if got := readMetadata("nobody"); got.PortBlock != nil || got.Name != "" {
		t.Errorf("missing metadata = %+v; want zero", got)
	}
	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, metadataFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readMetadata("crm"); got.PortBlock != nil {
		t.Errorf("garbage metadata = %+v; want no block", got)
	}
}

func TestSetMetadataBlockKeepsOtherFields(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(home, "crm", nil); err != nil {
		t.Fatal(err)
	}
	created := readMetadata("crm").CreatedAt

	if err := setMetadataBlock("crm", agentproto.PortBlock{Start: 16400, Size: 100}); err != nil {
		t.Fatal(err)
	}
	got := readMetadata("crm")
	if got.PortBlock == nil || got.PortBlock.Start != 16400 {
		t.Fatalf("block = %v", got.PortBlock)
	}
	if got.CreatedAt != created {
		t.Errorf("created_at changed: %q -> %q", created, got.CreatedAt)
	}
}

// The backfill has to work on a workspace whose metadata never existed — that is
// exactly the vintage of workspace it is for.
func TestSetMetadataBlockWithoutExistingFile(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	if err := os.MkdirAll(filepath.Join(base, "crm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setMetadataBlock("crm", agentproto.PortBlock{Start: 16500, Size: 100}); err != nil {
		t.Fatal(err)
	}
	got := readMetadata("crm")
	if got.PortBlock == nil || got.PortBlock.Start != 16500 || got.Name != "crm" {
		t.Errorf("reconstructed metadata = %+v", got)
	}
}

func TestWritePortsCmd(t *testing.T) {
	home := t.TempDir()
	if err := writePortsCmd(home); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, portsCmdRelPath)
	if got := string(mustRead(t, path)); got != portsCmdScript {
		t.Error("script content differs from the constant")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want executable", info.Mode().Perm())
	}
}

// The script is shipped as text and only ever runs on the server, so a syntax
// error would surface there, in a workspace, as a broken command — not here.
func TestPortsCmdScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "forge-ports")
	if err := os.WriteFile(path, []byte(portsCmdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v: %s", err, out)
	}
}

// With no block in the environment the command must say so and fail, rather than
// print a range of zeroes that a caller would then publish on.
func TestPortsCmdWithoutBlock(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "forge-ports")
	if err := os.WriteFile(path, []byte(portsCmdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, path)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("expected a non-zero exit, got:\n%s", out)
	}
	if !strings.Contains(string(out), "no port block") {
		t.Errorf("output should explain the missing block, got:\n%s", out)
	}
}

func TestEnsurePortsCmdBackfills(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// seedPortsCmd chowns, which needs root; ensurePortsCmd is best-effort and
	// writes the file before the chown fails, which is what we check.
	ensurePortsCmd("crm")
	if got := string(mustRead(t, filepath.Join(home, portsCmdRelPath))); got != portsCmdScript {
		t.Error("command not installed into a workspace that lacked it")
	}
}

// runPortsCmd runs the script with a stub `docker` on PATH, so the parsing that
// decides which ports are taken is exercised rather than assumed.
func runPortsCmd(t *testing.T, dockerStub string, env ...string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(dockerStub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "forge-ports")
	if err := os.WriteFile(script, []byte(portsCmdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, script)
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The stub answers `ps -aq` with two container ids and `inspect` with their host
// ports — one inside the block, one outside, and one from a container that is
// stopped (which `docker ps --format {{.Ports}}` would not have reported at all).
const dockerStub = `#!/bin/sh
case "$1" in
	ps) echo "aaa
bbb" ;;
	inspect) echo "16000 16001 3000 " ;;
esac
`

func TestPortsCmdReportsUsedFreeAndStray(t *testing.T) {
	out, err := runPortsCmd(t, dockerStub,
		"FORGE_PORT_MIN=16000", "FORGE_PORT_MAX=16099", "COMPOSE_PROJECT_NAME=crm")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{
		"range 16000-16099",
		"used  16000 16001",
		"free  16002 16003 16004 16005 16006",
		"stray 3000 — outside this block",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A docker that cannot be reached must fail loudly. Reporting "nothing is using
// ports" would invite the caller to publish on one that is already bound.
func TestPortsCmdFailsWhenDockerFails(t *testing.T) {
	const broken = "#!/bin/sh\nexit 1\n"
	out, err := runPortsCmd(t, broken,
		"FORGE_PORT_MIN=16000", "FORGE_PORT_MAX=16099", "COMPOSE_PROJECT_NAME=crm")
	if err == nil {
		t.Errorf("expected failure, got:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("should say the answer is unknown, got:\n%s", out)
	}
}

// An empty block reports every port free rather than printing nothing.
func TestPortsCmdWithNothingRunning(t *testing.T) {
	const empty = "#!/bin/sh\ncase \"$1\" in ps) : ;; esac\n"
	out, err := runPortsCmd(t, empty,
		"FORGE_PORT_MIN=16200", "FORGE_PORT_MAX=16299", "COMPOSE_PROJECT_NAME=crm")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(out, "free  16200 16201") || !strings.Contains(out, "used  (none)") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// The sweep restores a section somebody deleted, and leaves a workspace with no
// block alone rather than inventing a range for it.
func TestEnsurePortsMemory(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(home, "crm", &agentproto.PortBlock{Start: 16600, Size: 100}); err != nil {
		t.Fatal(err)
	}
	ensurePortsMemory("crm")
	if got := string(mustRead(t, filepath.Join(home, memoryRelPath))); !strings.Contains(got, "16600–16699") {
		t.Errorf("section not restored:\n%s", got)
	}

	blockless := filepath.Join(base, "shop")
	if err := os.MkdirAll(blockless, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(blockless, "shop", nil); err != nil {
		t.Fatal(err)
	}
	ensurePortsMemory("shop")
	if _, err := os.Stat(filepath.Join(blockless, memoryRelPath)); err == nil {
		t.Error("a workspace with no block should get no ports memory")
	}
}
