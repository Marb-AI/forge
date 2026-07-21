package agentproto

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The launch commands are shared by both front ends (the CLI and the browser UI)
// and are executed verbatim on the server. A typo here breaks both at once and
// only shows up on a live host, so pin their shape here.

func TestSourceEnvIsExportedAndTolerant(t *testing.T) {
	// `set -a` is what makes the sourced vars exported into the session.
	if !strings.HasPrefix(SourceEnv, "set -a;") {
		t.Errorf("SourceEnv must start by enabling auto-export, got %q", SourceEnv)
	}
	if !strings.Contains(SourceEnv, "set +a") {
		t.Error("SourceEnv must turn auto-export back off")
	}
	// A workspace with no env file must still start.
	if !strings.Contains(SourceEnv, `[ -f "$HOME/.forge/env" ]`) {
		t.Errorf("SourceEnv must tolerate a missing env file, got %q", SourceEnv)
	}
}

func TestLaunchCommandsSourceTheEnv(t *testing.T) {
	for name, cmd := range map[string]string{
		"AttachClaude": AttachClaude,
		"StartClaude":  StartClaude,
		"ResumeClaude": ResumeClaude("w17-01", "2026-07-20 14:03"),
	} {
		if !strings.HasPrefix(cmd, SourceEnv) {
			t.Errorf("%s must source the workspace env first, got %q", name, cmd)
		}
		if !strings.Contains(cmd, "-s "+TmuxSession) {
			t.Errorf("%s must target the %q tmux session, got %q", name, TmuxSession, cmd)
		}
	}
}

func TestAttachClaudeIsAttachOrCreate(t *testing.T) {
	// -A is the whole persistence story: attach if it's there, create if not.
	if !strings.Contains(AttachClaude, "tmux new -A -s "+TmuxSession+" claude") {
		t.Errorf("AttachClaude must attach-or-create, got %q", AttachClaude)
	}
}

func TestStartAndResumeAreDetached(t *testing.T) {
	// Nobody is attached when these run, so they must not try to take a terminal.
	if !strings.Contains(StartClaude, "tmux new -d -s "+TmuxSession+" claude") {
		t.Errorf("StartClaude must start detached, got %q", StartClaude)
	}
	resume := ResumeClaude("w17-01", "2026-07-20 14:03")
	if !strings.Contains(resume, "tmux new -d -s "+TmuxSession) {
		t.Errorf("ResumeClaude must start detached, got %q", resume)
	}
	// Resume is what a checkpoint restarts into — it must actually tell Claude to
	// pick the handoff back up, or the checkpoint silently loses its point.
	if !strings.Contains(resume, "continue from memory") {
		t.Errorf("ResumeClaude must continue from memory, got %q", resume)
	}
}

func TestKillClaudeIsIdempotent(t *testing.T) {
	if !strings.Contains(KillClaude, "kill-session -t "+TmuxSession) {
		t.Errorf("KillClaude must kill the %q session, got %q", TmuxSession, KillClaude)
	}
	// Stopping an already-stopped session is a no-op success; only a connection
	// failure should surface as an error to the caller.
	if !strings.Contains(KillClaude, "|| true") {
		t.Errorf("KillClaude must succeed when there is no session, got %q", KillClaude)
	}
}

// The resume command is assembled here and executed on a server, where it passes
// through ssh's shell, then the one tmux starts, before Claude finally sees its
// arguments. Three layers of quoting, and a prompt containing an em dash, a
// colon and (via the workspace name) whatever the user called it. So run the real
// string through a real shell and look at what actually arrives.
func TestResumeCommandSurvivesEveryShellLayer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		supportsName bool
		label        string
		wantArgv     []string
	}{{
		name:         "claude that knows --name",
		supportsName: true,
		wantArgv: []string{"-n", "w17-01 2026-07-20 14:03",
			"w17-01 2026-07-20 14:03 — continue from memory: read the handoff you just wrote and carry on from it."},
	}, {
		// An older Claude: the flag must not be passed, and the title still has to
		// come out distinguishable — here from the prompt's leading words.
		name:         "claude too old for --name",
		supportsName: false,
		wantArgv: []string{
			"w17-01 2026-07-20 14:03 — continue from memory: read the handoff you just wrote and carry on from it."},
	}, {
		// The label is now a few words from Claude about what the session was
		// about, so an apostrophe in it is ordinary, not exotic.
		name:         "topic with an apostrophe",
		supportsName: true,
		label:        "Claude's memory handoff",
		wantArgv: []string{"-n", "w17-01 Claude's memory handoff",
			"w17-01 Claude's memory handoff — continue from memory: read the handoff you just wrote and carry on from it."},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")

			help := ":" // a no-op, so the `then` branch is never empty
			if tc.supportsName {
				help = `echo "  -n, --name <name>    Set a display name for this session"`
			}
			write(t, filepath.Join(dir, "claude"), "#!/bin/sh\n"+
				"if [ \"$1\" = \"--help\" ]; then "+help+"; exit 0; fi\n"+
				"printf '%s\\n' \"$@\" > "+argvFile+"\n")
			// tmux runs the command string it is handed through a shell; that is the
			// layer this test exists to exercise, so emulate exactly that.
			write(t, filepath.Join(dir, "tmux"), "#!/bin/sh\n"+
				"for a in \"$@\"; do last=\"$a\"; done\n"+
				"exec sh -c \"$last\"\n")

			label := tc.label
			if label == "" {
				label = "2026-07-20 14:03"
			}
			cmd := exec.Command("sh", "-c", ResumeClaude("w17-01", label))
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("running the resume command failed: %v\n%s", err, out)
			}

			raw, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("claude was never invoked: %v", err)
			}
			got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if !slices.Equal(got, tc.wantArgv) {
				t.Errorf("claude received\n  %q\nwant\n  %q", got, tc.wantArgv)
			}
		})
	}
}

// A workspace name is [A-Za-z0-9_-], but the quoting must not be relying on that:
// it is the last thing between a prompt and the remote shell.
func TestResumeQuotingIsNotFooledByShellMetacharacters(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	write(t, filepath.Join(dir, "claude"), "#!/bin/sh\n"+
		"if [ \"$1\" = \"--help\" ]; then exit 0; fi\n"+
		"printf '%s\\n' \"$@\" > "+argvFile+"\n")
	write(t, filepath.Join(dir, "tmux"), "#!/bin/sh\n"+
		"for a in \"$@\"; do last=\"$a\"; done\nexec sh -c \"$last\"\n")
	canary := filepath.Join(dir, "pwned")

	nasty := `it's; touch ` + canary + `; echo $(whoami) "` + "`id`" + `"`
	cmd := exec.Command("sh", "-c", ResumeClaude(nasty, "2026-07-20 14:03"))
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("the workspace name escaped its quotes and ran a command")
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("claude was never invoked: %v", err)
	}
	if !strings.Contains(string(raw), nasty) {
		t.Errorf("the name should arrive intact and inert, got %q", raw)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
