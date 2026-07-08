// Command forge-agent is the privileged server-side helper, invoked over SSH by
// the forge CLI (typically as `sudo forge-agent <op>`). It is Linux-only by
// design and prints JSON on stdout.
package main

import (
	"os"

	"github.com/Marb-AI/forge/internal/agent"
)

func main() {
	os.Exit(agent.Main(os.Args[1:]))
}
