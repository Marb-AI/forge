package cli

import (
	"fmt"
	"strings"

	"github.com/Marb-AI/forge/forge"
)

// pairCmd is `forge pair`, and it is two commands wearing one name because it is
// one act seen from two machines: `forge pair <key>` on the device that is
// already in, `forge pair --accept <pairing>` on the one that is not.
//
// Naming them together is deliberate. Whoever runs the first is about to need
// the second, on a machine they are holding, and a command they can guess is
// better than one they have to look up while retyping a key.
func pairCmd(args []string) int {
	if len(args) == 0 {
		return fail("usage: forge pair <public key>  |  forge pair --accept <pairing>")
	}
	if args[0] == "--accept" || args[0] == "accept" {
		return pairAccept(args[1:])
	}
	return pairLetIn(args)
}

// pairLetIn runs on the device that already reaches the servers.
//
// The key is taken as the rest of the line rather than one argument, because
// what somebody pastes is three words with spaces in it and quoting that
// correctly is a thing to get wrong at the exact moment they are copying between
// two machines.
func pairLetIn(args []string) int {
	pairing, err := forge.LetDeviceIn(strings.Join(args, " "))
	if err != nil {
		return fail("%v", err)
	}
	encoded, err := pairing.Encode()
	if err != nil {
		return fail("%v", err)
	}

	fmt.Printf("forge: let it onto %d server(s)\n", len(pairing.Hosts))
	for _, h := range pairing.Hosts {
		fmt.Printf("  %-14s %s@%s\n", h.Alias, h.User, h.Addr)
	}
	fmt.Println("\nOn the other device:")
	fmt.Printf("\n  forge pair --accept %s\n", encoded)
	return 0
}

// pairAccept runs on the new device, and records what the other one said.
func pairAccept(args []string) int {
	if len(args) != 1 {
		return fail("usage: forge pair --accept <pairing>")
	}
	added, known, err := forge.AcceptPairing(args[0])
	if err != nil {
		return fail("%v", err)
	}
	for _, alias := range added {
		fmt.Printf("  %-14s added\n", alias)
	}
	for _, alias := range known {
		// Left alone rather than overwritten: the two may disagree, and pairing is
		// how a device hears about servers it does not know — not how one machine's
		// idea of a server replaces another's.
		fmt.Printf("  %-14s already known — left as it is\n", alias)
	}
	if len(added) == 0 {
		fmt.Println("forge: nothing new; this device already knew them all")
	}
	return 0
}
