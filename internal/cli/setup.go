package cli

import (
	"fmt"

	"github.com/Marb-AI/forge/forge"
)

// setupCmd is `forge setup`: the first thing you run, before there is a server
// to run it against.
//
// It prints the public half whether or not it made anything, because that is
// what the command is for — you come back to it every time you add a server, and
// a command that only tells you something the first time is a command you end up
// digging the answer out of a file for.
func setupCmd(args []string) int {
	// Takes nothing, and says so rather than ignoring it: every other command
	// here refuses argv it does not understand, and a typo that vanishes without
	// a word is the one that gets typed again.
	if len(args) > 0 {
		return fail("usage: forge setup (takes no arguments)")
	}
	pubkey, created, err := forge.Setup()
	if err != nil {
		return fail("%v", err)
	}
	if created {
		fmt.Println("forge: this device now has a key of its own")
	} else {
		fmt.Println("forge: this device already has a key — unchanged")
	}
	fmt.Printf("\n%s\n\n", pubkey)
	fmt.Println("Put that line where the server will read it:")
	fmt.Println("  about to be created — into its cloud-init, or the \"SSH keys\" field")
	fmt.Println("  already running     — append it to ~/.ssh/authorized_keys of the login")
	fmt.Println("                        Forge connects as (see: forge host list)")
	return 0
}
