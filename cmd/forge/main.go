// Command forge is the local CLI for managing remote Claude Code workspaces.
package main

import (
	"os"

	"github.com/Marb-AI/forge/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
