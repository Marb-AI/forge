package agentbin

import "testing"

// In a plain (non-release) build nothing is embedded, so Get errors — that's the
// signal callers use to fall back to a local file.
func TestGetStubErrors(t *testing.T) {
	if _, err := Get("amd64"); err == nil {
		t.Error("expected error from non-embedded build")
	}
}
