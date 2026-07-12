package sshx

import (
	"strings"
	"testing"

	"github.com/Marb-AI/forge/internal/config"
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
	joined := strings.Join(commonOpts(22), " ")
	if !strings.Contains(joined, "ConnectTimeout=") {
		t.Fatal("no ConnectTimeout: an unreachable host would hang every command that touches it")
	}
	if connectTimeout <= 0 || connectTimeout > 30 {
		t.Errorf("ConnectTimeout=%d is not a bound anyone would wait out", connectTimeout)
	}
}
