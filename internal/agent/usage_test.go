package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Marb-AI/forge/internal/agentproto"
)

// A statusLine payload shaped like the real thing, trimmed to the parts forge-usage
// reads plus a couple it must ignore. Claude Code sends a great deal more; the
// script has to take what it knows and leave the rest alone.
const statusLinePayload = `{
  "hook_event_name": "Status",
  "session_id": "8f96089e-1f8d-4d6f-b072-cc18fb6afac3",
  "model": {"id": "claude-opus-5", "display_name": "Opus 5"},
  "workspace": {"current_dir": "/home/workspaces/crm"},
  "cost": {"total_cost_usd": 1.2345, "total_lines_added": 156},
  "context_window": {
    "total_input_tokens": 128471,
    "total_output_tokens": 890,
    "context_window_size": 200000,
    "used_percentage": 64.2
  },
  "rate_limits": {
    "five_hour": {"used_percentage": 23.5, "resets_at": 1738425600},
    "seven_day": {"used_percentage": 41.2, "resets_at": 1738857600}
  }
}`

// The end of the loop that matters, as with the topic command: the python that
// writes the sample and the Go that reads it back are two pieces of text in two
// languages, and nothing else checks they agree on the format. This one also pins
// the OTHER contract — the field names Claude Code puts on stdin — which is the part
// no amount of Go-side testing would catch drifting.
func TestUsageCmdWritesWhatTheAgentReads(t *testing.T) {
	requirePython(t)
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeUsageCmd(home); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Unix()
	line := runUsageCmd(t, home, statusLinePayload)

	u, ok := readUsage("crm")
	if !ok {
		t.Fatal("readUsage: nothing readable after forge-usage ran")
	}
	if u.TS < before || u.TS > time.Now().Unix() {
		t.Errorf("sample stamped %d, outside the window [%d, %d] the command ran in",
			u.TS, before, time.Now().Unix())
	}
	if u.Model != "Opus 5" {
		t.Errorf("model = %q, want %q", u.Model, "Opus 5")
	}
	// Input tokens, not the percentage: the agent reports both halves so the UI can
	// say "128k of 200k" as well as "64%".
	if u.ContextUsed != 128471 || u.ContextSize != 200000 {
		t.Errorf("context = %d/%d, want 128471/200000", u.ContextUsed, u.ContextSize)
	}
	if u.CostUSD != 1.2345 {
		t.Errorf("cost = %v, want 1.2345", u.CostUSD)
	}
	if u.FiveHour == nil || u.SevenDay == nil {
		t.Fatalf("rate windows = %v/%v, want both present", u.FiveHour, u.SevenDay)
	}
	if u.FiveHour.UsedPercent != 23.5 || u.FiveHour.ResetsAt != 1738425600 {
		t.Errorf("five_hour = %+v, want 23.5%% resetting at 1738425600", *u.FiveHour)
	}
	if u.SevenDay.UsedPercent != 41.2 || u.SevenDay.ResetsAt != 1738857600 {
		t.Errorf("seven_day = %+v, want 41.2%% resetting at 1738857600", *u.SevenDay)
	}

	// It is a status line as well as a probe: somebody sitting in the tmux session
	// gets the same numbers, or this feature would be invisible where Claude runs.
	for _, want := range []string{"Opus 5", "ctx 64%", "5h 24%", "7d 41%"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line %q does not mention %q", line, want)
		}
	}
}

// An API-credit workspace gets no rate_limits at all — the field is Claude.ai
// subscribers only. That must come back as ABSENT, not as an untouched allowance:
// two empty bars reading 0% would tell a company on credits it has plenty of a
// window it does not have.
func TestUsageCmdReportsAbsentWindowsAsAbsent(t *testing.T) {
	requirePython(t)
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeUsageCmd(home); err != nil {
		t.Fatal(err)
	}

	runUsageCmd(t, home, `{"model":{"display_name":"Opus 5"},
		"context_window":{"total_input_tokens":1000,"context_window_size":200000},
		"cost":{"total_cost_usd":0.5}}`)

	u, ok := readUsage("crm")
	if !ok {
		t.Fatal("a payload with no rate limits is still a usable sample")
	}
	if u.FiveHour != nil || u.SevenDay != nil {
		t.Errorf("windows = %+v/%+v, want both nil for a payload that had none", u.FiveHour, u.SevenDay)
	}
	if u.ContextSize != 200000 {
		t.Errorf("context size = %d, want the rest of the sample to survive", u.ContextSize)
	}
}

// This runs on every render of somebody's live session. A payload it cannot make
// sense of must cost the numbers and nothing else — never the status line, and never
// an exit status Claude Code would surface as a broken configuration.
func TestUsageCmdSurvivesNonsense(t *testing.T) {
	requirePython(t)
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeUsageCmd(home); err != nil {
		t.Fatal(err)
	}

	for _, payload := range []string{
		"",
		"not json at all",
		`{"context_window": "a string where an object goes"}`,
		`{"rate_limits": {"five_hour": {"used_percentage": true}}}`,
		`{"model": {"display_name": null}, "cost": {"total_cost_usd": "free"}}`,
	} {
		runUsageCmd(t, home, payload) // fails the test if it exits non-zero
		u, ok := readUsage("crm")
		if !ok {
			t.Errorf("payload %q left no readable sample; a timestamp alone is still a sample", payload)
			continue
		}
		if u.FiveHour != nil {
			t.Errorf("payload %q invented a rate window: %+v", payload, *u.FiveHour)
		}
	}
}

func TestParseUsage(t *testing.T) {
	// What the script writes, byte for byte in shape.
	sample := `{"ts":1784386328,"model":"Opus 5","context_used":128471,"context_size":200000,
		"cost_usd":1.2345,"five_hour":{"used_percent":23.5,"resets_at":1738425600},"seven_day":null}`
	u, ok := parseUsage([]byte(sample))
	if !ok {
		t.Fatal("a sample the command wrote must parse")
	}
	if u.TS != 1784386328 || u.ContextUsed != 128471 || u.CostUSD != 1.2345 {
		t.Errorf("parsed %+v, want the values as written", u)
	}
	if u.FiveHour == nil || u.FiveHour.UsedPercent != 23.5 {
		t.Errorf("five_hour = %v, want 23.5%%", u.FiveHour)
	}
	// An explicit null is as absent as a missing key — one login can report the
	// 5-hour window and not the weekly one.
	if u.SevenDay != nil {
		t.Errorf("seven_day = %+v, want nil for an explicit null", *u.SevenDay)
	}
}

// A sample with no timestamp is not a sample. Every number in it is only meaningful
// as of some moment, and a group's freshness is the whole basis on which the UI
// decides whether to believe it.
func TestParseUsageRejectsUnstamped(t *testing.T) {
	for _, in := range []string{
		`{"context_used":128471,"context_size":200000}`, // no ts
		`{"ts":0,"context_used":1}`,                     // ts present but empty
		`{"ts":-5}`,                                     // nonsense ts
		`not json`,
		``,
	} {
		if u, ok := parseUsage([]byte(in)); ok {
			t.Errorf("parseUsage(%q) = (%+v, true), want it rejected", in, u)
		}
	}
}

// The file lives in a home directory whose user can write anything into it, and what
// it holds ends up in meters and in the browser's DOM. So text is flattened,
// percentages are held to being percentages, and impossible counts become zero
// rather than being rendered.
func TestParseUsageDistrustsTheFile(t *testing.T) {
	// A control character can only be in a JSON string escaped — one written raw
	// makes the whole file invalid, which is its own case below.
	u, ok := parseUsage([]byte(`{"ts":42,"model":"Opus\u001b[31m 5\nsecond line",
		"context_used":-9,"context_size":-1,"cost_usd":-3,
		"five_hour":{"used_percent":900,"resets_at":-7},
		"seven_day":{"used_percent":-20}}`))
	if !ok {
		t.Fatal("a stamped sample should still parse after sanitising")
	}
	if u.Model != "Opus [31m 5 second line" {
		t.Errorf("model = %q, want the escape stripped and the newline flattened", u.Model)
	}
	if u.ContextUsed != 0 || u.ContextSize != 0 || u.CostUSD != 0 {
		t.Errorf("negatives survived: %+v", u)
	}
	if u.FiveHour.UsedPercent != 100 || u.FiveHour.ResetsAt != 0 {
		t.Errorf("five_hour = %+v, want the percentage capped at 100 and the reset dropped", *u.FiveHour)
	}
	if u.SevenDay.UsedPercent != 0 {
		t.Errorf("seven_day = %+v, want a negative percentage floored at 0", *u.SevenDay)
	}

	// And a raw control byte inside a string is not valid JSON at all, so the sample
	// is refused whole rather than half-read. Worth pinning: it is the difference
	// between "we sanitise what we decode" and "we decode whatever arrives".
	if got, ok := parseUsage([]byte("{\"ts\":42,\"model\":\"Opus\x1b[31m\"}")); ok {
		t.Errorf("a file with a raw control byte parsed to %+v, want it rejected", got)
	}
}

// The login is read from ~/.claude.json, never from the sample. A workspace could
// otherwise name itself into somebody else's group — and the group is where the
// rate-limit figures are shown.
func TestParseUsageIgnoresAnAccountInTheSample(t *testing.T) {
	// A control character can only be in a JSON string escaped — one written raw
	// makes the whole file invalid, which is its own case below.
	u, ok := parseUsage([]byte(`{"ts":42,"account":{"uuid":"somebody-elses","email":"ceo@example.com"}}`))
	if !ok {
		t.Fatal("the sample is otherwise fine")
	}
	if u.Account != (agentproto.Account{}) {
		t.Errorf("account = %+v, want it ignored: only claude.json says who a workspace is", u.Account)
	}
}

func TestParseAccount(t *testing.T) {
	// The shape Claude Code writes, cut down: the real file holds a hundred other
	// keys and only this object is ours to read.
	claudeJSON := `{
	  "numStartups": 42,
	  "oauthAccount": {
	    "accountUuid": "dc77a89f-571a-445f-a863-bd0b3eca0691",
	    "emailAddress": "dev@example.com",
	    "displayName": "A Developer",
	    "organizationName": "Example Ltd",
	    "organizationRole": "admin"
	  }
	}`
	a, ok := parseAccount([]byte(claudeJSON))
	if !ok {
		t.Fatal("a signed-in claude.json must yield a login")
	}
	want := agentproto.Account{
		UUID:  "dc77a89f-571a-445f-a863-bd0b3eca0691",
		Email: "dev@example.com",
		Name:  "A Developer",
		Org:   "Example Ltd",
	}
	if a != want {
		t.Errorf("account = %+v, want %+v", a, want)
	}

	// A claude.json exists from the first run, long before anyone signs in — and an
	// API-credit workspace never signs in at all. "Has the file" and "has a login"
	// are different facts, and the id is what tells them apart.
	for _, in := range []string{
		`{"numStartups": 3}`,
		`{"oauthAccount": {}}`,
		`{"oauthAccount": {"emailAddress": "dev@example.com"}}`,
		`{"oauthAccount": "not an object"}`,
		`not json`,
	} {
		if a, ok := parseAccount([]byte(in)); ok {
			t.Errorf("parseAccount(%q) = (%+v, true), want no login", in, a)
		}
	}

	// Labels are held to a length and flattened, for the same reason a topic is:
	// they go in a group header in a narrow column, and nobody promised the file's
	// shape.
	long := strings.Repeat("é", agentproto.LabelMaxRunes+40)
	a, ok = parseAccount([]byte(`{"oauthAccount":{"accountUuid":"u","organizationName":"` + long + `"}}`))
	if !ok {
		t.Fatal("a long organisation name should not lose the login")
	}
	if runes := []rune(a.Org); len(runes) != agentproto.LabelMaxRunes+1 || runes[len(runes)-1] != '…' {
		t.Errorf("org cut to %d runes, want %d and an ellipsis", len([]rune(a.Org)), agentproto.LabelMaxRunes+1)
	}
}

// How a workspace pays decides which numbers mean anything — an organisation on API
// credits has no 5-hour window, and that absence is the nature of the thing rather
// than a gap in our reading. Detection is by inspection, in the order Claude Code
// itself resolves credentials.
func TestDetectAuth(t *testing.T) {
	cases := []struct {
		name       string
		forgeEnv   string
		settings   string
		hasAccount bool
		want       string
	}{
		{"a plain signed-in workspace", "", "", true, agentproto.AuthSubscription},
		{"nothing at all", "", "", false, agentproto.AuthUnknown},
		{"a key in the forge env", "ANTHROPIC_API_KEY=sk-ant-secret\n", "", false, agentproto.AuthAPIKey},
		{"a key in claude settings", "", `{"env":{"ANTHROPIC_API_KEY":"sk-ant-secret"}}`, false, agentproto.AuthAPIKey},
		{"an auth token", "ANTHROPIC_AUTH_TOKEN=t\n", "", false, agentproto.AuthAPIKey},
		{"a key helper", "", `{"apiKeyHelper":"/usr/local/bin/get-key"}`, false, agentproto.AuthAPIKey},
		{"bedrock", "CLAUDE_CODE_USE_BEDROCK=1\n", "", false, agentproto.AuthBedrock},
		{"vertex", "CLAUDE_CODE_USE_VERTEX=true\n", "", false, agentproto.AuthVertex},
		// The provider wins over a login on file: a workspace can hold a perfectly
		// good oauthAccount from months ago and still bill every token to Bedrock.
		{"bedrock over a stale login", "CLAUDE_CODE_USE_BEDROCK=1\n", "", true, agentproto.AuthBedrock},
		{"a key over a stale login", "ANTHROPIC_API_KEY=sk-ant-secret\n", "", true, agentproto.AuthAPIKey},
		// A flag set to zero is a decision, not a switch.
		{"bedrock switched off", "CLAUDE_CODE_USE_BEDROCK=0\n", "", true, agentproto.AuthSubscription},
		{"an empty key is no key", "ANTHROPIC_API_KEY=\n", "", true, agentproto.AuthSubscription},
		// The env file is written with `set -a` in mind and quoted values are ordinary.
		{"a quoted key", "ANTHROPIC_API_KEY=\"sk-ant-secret\"\n", "", false, agentproto.AuthAPIKey},
		{"comments and blanks are skipped", "# ANTHROPIC_API_KEY=sk-ant\n\n", "", false, agentproto.AuthUnknown},
	}
	for _, c := range cases {
		base := t.TempDir()
		home := filepath.Join(base, "crm")
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".forge"), 0o755); err != nil {
			t.Fatal(err)
		}
		if c.forgeEnv != "" {
			writeFile(t, filepath.Join(home, envRelPath), c.forgeEnv)
		}
		if c.settings != "" {
			writeFile(t, filepath.Join(home, ".claude", "settings.json"), c.settings)
		}
		func() {
			defer func(old string) { baseDir = old }(baseDir)
			baseDir = base
			if got := detectAuth("crm", c.hasAccount); got != c.want {
				t.Errorf("%s: detectAuth = %q, want %q", c.name, got, c.want)
			}
		}()
	}
}

// A workspace made before this existed gets the command and the settings entry on
// the next usage sweep, with no re-provision — contents, not existence, so a change
// to either still lands.
func TestEnsureUsageBackfills(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	home := filepath.Join(base, "crm")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	ensureUsageCmd("crm")
	script := filepath.Join(home, usageCmdRelPath)
	if got := string(mustRead(t, script)); got != usageCmdScript {
		t.Fatal("ensureUsageCmd did not install the command")
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("forge-usage is not executable (mode %v) — the status line would fail every render",
			info.Mode().Perm())
	}

	ensureUsageStatusLine("crm")
	line := statusLineOf(t, filepath.Join(home, ".claude", "settings.json"))
	if cmd, _ := line["command"].(string); cmd != usageStatusLineCmd {
		t.Errorf("statusLine command = %q, want %q", cmd, usageStatusLineCmd)
	}
	// Without the timer the numbers stop moving the moment the session goes quiet,
	// which is exactly when a stale rate limit is most misleading.
	if n, _ := line["refreshInterval"].(float64); int(n) != usageRefreshSeconds {
		t.Errorf("refreshInterval = %v, want %d", line["refreshInterval"], usageRefreshSeconds)
	}

	// An out-of-date entry from an earlier version is brought up to date.
	writeFile(t, filepath.Join(home, ".claude", "settings.json"),
		`{"statusLine":{"type":"command","command":`+usageStatusLineCmd+`,"refreshInterval":600}}`)
	ensureUsageStatusLine("crm")
	line = statusLineOf(t, filepath.Join(home, ".claude", "settings.json"))
	if n, _ := line["refreshInterval"].(float64); int(n) != usageRefreshSeconds {
		t.Errorf("a stale refreshInterval was left at %v, want it refreshed to %d",
			line["refreshInterval"], usageRefreshSeconds)
	}
}

// Claude's status line is a single slot, and one somebody deliberately configured is
// theirs. Forge does not take it — that workspace reports no sample, which is a
// smaller loss than silently replacing a row of somebody's screen. (Contrast the
// activity hooks, which are appended: there is room for everyone's.)
func TestEnsureUsageStatusLineLeavesTheirsAlone(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	settings := filepath.Join(base, "crm", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := `{"statusLine":{"type":"command","command":"~/bin/my-prompt.sh"},` +
		`"permissions":{"defaultMode":"bypassPermissions"}}`
	writeFile(t, settings, theirs)

	ensureUsageStatusLine("crm")
	if got := string(mustRead(t, settings)); got != theirs {
		t.Errorf("settings.json was rewritten:\n%s", got)
	}

	// Nor is a settings.json we cannot parse replaced wholesale — losing somebody's
	// configuration costs more than this workspace's usage numbers are worth.
	broken := "{ this is not json"
	writeFile(t, settings, broken)
	ensureUsageStatusLine("crm")
	if got := string(mustRead(t, settings)); got != broken {
		t.Errorf("a malformed settings.json was overwritten:\n%s", got)
	}
}

// The sweep is on a timer, so anything it writes it writes forever. Once the entry
// is in place and current, it must stop touching the file: settings.json is the
// user's, and a workspace whose config changes mtime every ten seconds is a
// workspace nobody can reason about.
func TestEnsureUsageStatusLineStopsWriting(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	settings := filepath.Join(base, "crm", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	ensureUsageStatusLine("crm")
	first := mustRead(t, settings)
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(settings, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	ensureUsageStatusLine("crm")
	info, err := os.Stat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Error("a settings.json already holding our current entry was written again")
	}
	if got := mustRead(t, settings); string(got) != string(first) {
		t.Errorf("contents changed on a second pass:\n%s", got)
	}
}

// A stopped workspace still answers "which login is this one on" — the sample is
// gone but the login is not, and that question is half the reason to show it. The
// reverse also holds: an API-credit workspace has no login and must still appear.
func TestOpUsageReportsEitherHalf(t *testing.T) {
	base := t.TempDir()
	defer func(old string) { baseDir = old }(baseDir)
	baseDir = base

	// Signed in, never rendered: login only.
	signedIn := filepath.Join(base, "signedin")
	if err := os.MkdirAll(filepath.Join(signedIn, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(signedIn, agentproto.AccountFile),
		`{"oauthAccount":{"accountUuid":"u-1","emailAddress":"dev@example.com"}}`)

	// On credits: no login, no windows, but a sample and an auth kind.
	credits := filepath.Join(base, "credits")
	if err := os.MkdirAll(filepath.Join(credits, ".forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(credits, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(credits, envRelPath), "ANTHROPIC_API_KEY=sk-ant-secret\n")
	writeFile(t, filepath.Join(credits, agentproto.UsageFile),
		`{"ts":1784386328,"context_used":1000,"context_size":200000,"cost_usd":2.5}`)

	// Nobody has ever run anything here: no login, no sample, nothing to say.
	if err := os.MkdirAll(filepath.Join(base, "fresh"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := captureUsage(t)
	if len(res.Usage) != 2 {
		t.Fatalf("reported %d workspaces (%v), want the two that have something to say",
			len(res.Usage), res.Usage)
	}
	if got := res.Usage["signedin"]; got.Account.Email != "dev@example.com" ||
		got.Auth != agentproto.AuthSubscription || got.TS != 0 {
		t.Errorf("signed-in workspace = %+v, want the login, subscription auth and no sample", got)
	}
	if got := res.Usage["credits"]; got.Account.UUID != "" ||
		got.Auth != agentproto.AuthAPIKey || got.CostUSD != 2.5 {
		t.Errorf("credit workspace = %+v, want no login, api auth and its spend", got)
	}
	if _, ok := res.Usage["fresh"]; ok {
		t.Error("a workspace with no login and no sample should have no entry")
	}
}

// captureUsage runs opUsage and decodes what it printed, the same way the CLI does
// over SSH.
func captureUsage(t *testing.T) agentproto.UsageResult {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := opUsage()
	w.Close()
	os.Stdout = old
	if code != 0 {
		t.Fatalf("opUsage exited %d", code)
	}
	var res agentproto.UsageResult
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		t.Fatalf("decode opUsage output: %v", err)
	}
	return res
}

// runUsageCmd feeds a statusLine payload to the installed command and returns what
// it printed. A non-zero exit is a failure in itself: Claude Code runs this on every
// render, and a command that errors is a broken status line.
func runUsageCmd(t *testing.T, home, payload string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(home, usageCmdRelPath))
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forge-usage on %q: %v: %s", payload, err, out)
	}
	return string(out)
}

// statusLineOf reads back the statusLine entry of a settings.json.
func statusLineOf(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(mustRead(t, path), &m); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	line, ok := m["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("no statusLine in %s", path)
	}
	return line
}

// The status line command is python, like the activity hooks — every Forge host has
// it (`host prepare` installs one), but a developer's laptop running the tests need
// not.
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed: the status line command cannot be exercised here")
	}
}
