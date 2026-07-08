//go:build embedagent

package agentbin

import (
	_ "embed"
	"fmt"
)

//go:embed forge-agent-linux-amd64
var amd64Bin []byte

//go:embed forge-agent-linux-arm64
var arm64Bin []byte

// Get returns the embedded forge-agent for the given GOARCH.
func Get(goarch string) ([]byte, error) {
	switch goarch {
	case "amd64":
		return amd64Bin, nil
	case "arm64":
		return arm64Bin, nil
	default:
		return nil, fmt.Errorf("no embedded agent for arch %q", goarch)
	}
}
