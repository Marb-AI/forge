package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocateAgentBinaryFromEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "forge-agent-linux-amd64")
	if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_AGENT_BIN", p)
	got, err := locateAgentBinary("amd64")
	if err != nil || got != p {
		t.Fatalf("locateAgentBinary = (%q,%v), want %q", got, err, p)
	}
}

func TestLocateAgentBinaryEnvMissing(t *testing.T) {
	t.Setenv("FORGE_AGENT_BIN", filepath.Join(t.TempDir(), "nope"))
	if _, err := locateAgentBinary("amd64"); err == nil {
		t.Error("expected error for missing FORGE_AGENT_BIN")
	}
}

func TestAgentReaderFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "forge-agent-linux-arm64")
	if err := os.WriteFile(p, []byte("AGENT"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_AGENT_BIN", p)

	// In a dev build agentbin.Get errors, so agentReader falls back to the file.
	r, label, closeFn, err := agentReader("arm64")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if label != "forge-agent-linux-arm64" {
		t.Errorf("label = %q", label)
	}
	data, _ := io.ReadAll(r)
	if string(data) != "AGENT" {
		t.Errorf("content = %q", data)
	}
}
