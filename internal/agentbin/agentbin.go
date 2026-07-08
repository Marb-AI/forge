//go:build !embedagent

// Package agentbin provides the forge-agent binary that `forge host prepare`
// uploads to a server. In a plain dev build nothing is embedded (this stub);
// release builds (`make release`, which sets -tags embedagent) embed the
// cross-compiled linux binaries so a single released `forge` carries the agent
// for every server arch. Callers fall back to a local file when Get errors.
package agentbin

import "errors"

// Get returns the embedded forge-agent for the given GOARCH ("amd64"/"arm64").
func Get(goarch string) ([]byte, error) {
	return nil, errors.New("agent binary not embedded in this build")
}
