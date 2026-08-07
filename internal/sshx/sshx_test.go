package sshx

import (
	"slices"
	"strings"
	"testing"

	"github.com/Marb-AI/forge/config"
)

func joined(args []string) string { return strings.Join(args, " ") }

func TestArgsDefaultPort(t *testing.T) {
	tgt := Target{User: "crm", Addr: "1.2.3.4", Port: 22}
	got := joined(tgt.Args("tmux", "ls"))
	if strings.Contains(got, "-p ") {
		t.Errorf("port 22 should not add -p: %s", got)
	}
	if !strings.HasSuffix(got, "crm@1.2.3.4 tmux ls") {
		t.Errorf("unexpected args: %s", got)
	}
	if !strings.Contains(got, "ServerAliveInterval=5") {
		t.Errorf("missing keepalive: %s", got)
	}
}

func TestArgsCustomPort(t *testing.T) {
	tgt := Target{User: "root", Addr: "h", Port: 2222}
	got := joined(tgt.Args("id"))
	if !strings.Contains(got, "-p 2222") {
		t.Errorf("expected -p 2222: %s", got)
	}
}

func TestTTYArgsHasT(t *testing.T) {
	tgt := Target{User: "u", Addr: "h", Port: 22}
	got := tgt.TTYArgs("bash")
	if got[0] != "-t" {
		t.Errorf("TTYArgs should start with -t: %v", got)
	}
}

func TestLocalForwardArgs(t *testing.T) {
	tgt := Target{User: "crm", Addr: "h", Port: 22}
	got := joined(tgt.LocalForwardArgs(3050, 3000))
	if !strings.Contains(got, "-L 3050:localhost:3000") {
		t.Errorf("bad forward spec: %s", got)
	}
	if !strings.Contains(got, "-N") || !strings.Contains(got, "ExitOnForwardFailure=yes") {
		t.Errorf("missing -N / ExitOnForwardFailure: %s", got)
	}
}

func TestTargetsFromHost(t *testing.T) {
	h := &config.Host{User: "admin", Addr: "srv", Port: 2200}
	if a := AdminTarget(h); a.User != "admin" || a.Addr != "srv" || a.Port != 2200 {
		t.Errorf("AdminTarget = %+v", a)
	}
	if w := WorkspaceTarget(h, "crm"); w.User != "crm" || w.Addr != "srv" {
		t.Errorf("WorkspaceTarget = %+v", w)
	}
}

// Without ConnectTimeout, ssh waits out the operating system's TCP timeout —
// measured at over 45 seconds against an unreachable address — and every command
// that touches that host waits with it, the browser UI's workspace list included.
// ServerAlive* does not cover this: it only notices a peer that dies *after* the
// connection is established. A host that never answers at all is this option's job.
func TestEveryConnectionBoundsHowLongItWaitsForTheServer(t *testing.T) {
	joined := strings.Join(commonOpts(Target{User: "u", Addr: "h", Port: 22}), " ")
	if !strings.Contains(joined, "ConnectTimeout=") {
		t.Fatal("no ConnectTimeout: an unreachable host would hang every command that touches it")
	}
	if connectTimeout <= 0 || connectTimeout > 30 {
		t.Errorf("ConnectTimeout=%d is not a bound anyone would wait out", connectTimeout)
	}
}

// This file has always said Forge is key-only and never prompts for a password.
// It wasn't: BatchMode=no is the default and does nothing to stop password auth —
// `ssh -G` reported passwordauthentication yes the entire time the comment claimed
// otherwise. A bad key would drop into a prompt, which in the UI daemon is a prompt
// nobody is there to answer.
func TestKeyOnlyIsEnforcedAndNotMerelyClaimed(t *testing.T) {
	joined := strings.Join(commonOpts(Target{User: "u", Addr: "h", Port: 22}), " ")

	for _, off := range []string{"PasswordAuthentication=no", "KbdInteractiveAuthentication=no"} {
		if !strings.Contains(joined, off) {
			t.Errorf("key-only is claimed but not enforced: missing %s", off)
		}
	}
	// …while a *local* key passphrase must still be askable. That is a different
	// thing from the server asking for a password, and BatchMode=yes would break it.
	if !strings.Contains(joined, "BatchMode=no") {
		t.Error("BatchMode must stay no, or a passphrase-protected key can't be used")
	}
}

// A hop written without a login takes the host's own, and the completed route is
// what both clients are handed — see jumpChain for why that matters more than it
// looks: ssh would fill an implicit login in from this machine's username, and
// the Go client from a default of its own.
func TestAHopWithoutALoginTakesTheHosts(t *testing.T) {
	for _, c := range []struct {
		name string
		host *config.Host
		want string
	}{
		{"no jump", &config.Host{User: "admin", Addr: "srv"}, ""},
		{"bare host", &config.Host{User: "admin", Addr: "srv", Jump: "bastion"}, "admin@bastion"},
		{"login given", &config.Host{User: "admin", Addr: "srv", Jump: "jump@bastion"}, "jump@bastion"},
		{"port given", &config.Host{User: "admin", Addr: "srv", Jump: "bastion:2222"}, "admin@bastion:2222"},
		{"a chain", &config.Host{User: "admin", Addr: "srv", Jump: "one, two@2.2.2.2"}, "admin@one,two@2.2.2.2"},
	} {
		if got := jumpChain(c.host); got != c.want {
			t.Errorf("%s: jumpChain = %q, want %q", c.name, got, c.want)
		}
	}
	// And it is on both kinds of target, because a workspace is reached through
	// the same servers its host is — logging in as a name no bastion has heard of.
	h := &config.Host{User: "admin", Addr: "srv", Jump: "bastion"}
	if got := WorkspaceTarget(h, "crm").Jump; got != "admin@bastion" {
		t.Errorf("WorkspaceTarget jump = %q, want the host's route", got)
	}
}

// The one string, read two ways: ssh gets it after -J, and the pure-Go client
// parses it into hops. A route that means one thing to one client and something
// else to the other is the failure this rules out.
func TestBothClientsAreGivenTheSameRoute(t *testing.T) {
	tgt := AdminTarget(&config.Host{User: "admin", Addr: "srv", Port: 22, Jump: "bastion:2222,two@2.2.2.2"})

	// Every argv shape carries it: a terminal or a tunnel that forgot the route
	// would hang against a server that is not reachable without it.
	for name, args := range map[string][]string{
		"Args":             tgt.Args("id"),
		"TTYArgs":          tgt.TTYArgs("bash"),
		"LocalForwardArgs": tgt.LocalForwardArgs(3050, 3000),
	} {
		if !strings.Contains(joined(args), "-J "+tgt.Jump) {
			t.Errorf("%s does not carry the route: %s", name, joined(args))
		}
	}

	hops, err := ParseJump(tgt.Jump)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 {
		t.Fatalf("ParseJump(%q) = %d hops, want 2", tgt.Jump, len(hops))
	}
	if hops[0].User != "admin" || hops[0].Addr != "bastion" || hops[0].Port != 2222 {
		t.Errorf("first hop = %+v", hops[0])
	}
	if hops[1].User != "two" || hops[1].Addr != "2.2.2.2" || hops[1].Port != 22 {
		t.Errorf("second hop = %+v", hops[1])
	}
}

// A route that cannot be read is refused where it is written (forge.AddHost),
// rather than at the first connection — by which time the message would be about
// a host that could not be reached, and about nothing that could be corrected.
func TestParseJumpRefusesWhatCannotBeARoute(t *testing.T) {
	for _, spec := range []string{"one,,two", "bastion:ssh", ","} {
		if _, err := ParseJump(spec); err == nil {
			t.Errorf("ParseJump(%q) was accepted", spec)
		}
	}
	if hops, err := ParseJump("  "); err != nil || hops != nil {
		t.Errorf("ParseJump(\"  \") = %v, %v; want no hops and no error", hops, err)
	}
}

// Every argv this package builds carries the device key, because the workspaces
// Forge makes admit that key and nothing else — an ssh offering ~/.ssh would be
// turned away from a workspace this very client had just created.
//
// IdentitiesOnly with it: -i only ADDS to what ssh would try, so without it a
// running agent still gets first refusal and the key that matters may never be
// offered at all.
func TestEveryInteractiveArgvCarriesTheDeviceKey(t *testing.T) {
	useIdentityPath(t, "/state/id.pem")

	tgt := Target{User: "crm", Addr: "h", Port: 22}
	for name, args := range map[string][]string{
		"Args":             tgt.Args("id"),
		"TTYArgs":          tgt.TTYArgs("bash"),
		"LocalForwardArgs": tgt.LocalForwardArgs(3050, 3000),
	} {
		got := joined(args)
		if !strings.Contains(got, "-i /state/id.pem") {
			t.Errorf("%s offers no identity: %s", name, got)
		}
		if !strings.Contains(got, "IdentitiesOnly=yes") {
			t.Errorf("%s lets ssh offer other keys as well: %s", name, got)
		}
	}
}

// A device whose key is somewhere a path cannot describe — a Keychain, a chip —
// gets no -i, and nothing breaks: there is no ssh binary on such a device for
// the argv to reach.
func TestNoIdentityFileMeansNoArgumentForIt(t *testing.T) {
	useIdentitySeam(t, nil, nil)

	if got := joined(Target{User: "u", Addr: "h"}.Args("id")); strings.Contains(got, "-i ") {
		t.Errorf("argv names a key file this device does not have: %s", got)
	}
}

// Dropping -A stops Forge asking for agent forwarding. It does not stop the
// user's own ~/.ssh/config turning it on.
//
// `ForwardAgent yes` for a host in that file applies to every ssh to it,
// including these, and the agent would be on the far end with nothing here
// having mentioned it — while the Go client, which cannot be configured into it,
// would not do the same. The two backends have to mean the same thing.
//
// The reason it matters is 2.2: git on the server runs under the identity `host
// prepare` put there, and an agent on top of it only obscures who is pushing.
func TestNoArgvAsksForAnAgentOrLetsOneBeConfiguredIn(t *testing.T) {
	target := Target{User: "ws", Addr: "10.0.0.1"}

	for name, args := range map[string][]string{
		"Args":             target.Args("true"),
		"TTYArgs":          target.TTYArgs(),
		"LocalForwardArgs": target.LocalForwardArgs(16000, 16000),
	} {
		joined := strings.Join(args, " ")
		if slices.Contains(args, "-A") {
			t.Errorf("%s asks for agent forwarding: %s", name, joined)
		}
		if !strings.Contains(joined, "ForwardAgent=no") {
			t.Errorf("%s leaves agent forwarding to ~/.ssh/config, which can turn it "+
				"on for this host: %s", name, joined)
		}
	}
}
