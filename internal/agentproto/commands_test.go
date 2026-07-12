package agentproto

import "strings"

import "testing"

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
		"ResumeClaude": ResumeClaude,
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
	if !strings.Contains(ResumeClaude, "tmux new -d -s "+TmuxSession) {
		t.Errorf("ResumeClaude must start detached, got %q", ResumeClaude)
	}
	// Resume is what a checkpoint restarts into — it must actually tell Claude to
	// pick the handoff back up, or the checkpoint silently loses its point.
	if !strings.Contains(ResumeClaude, `claude "continue from memory"`) {
		t.Errorf("ResumeClaude must continue from memory, got %q", ResumeClaude)
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
