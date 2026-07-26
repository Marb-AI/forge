package sshx

import (
	"errors"
	"os/exec"

	"github.com/Marb-AI/forge/internal/localpty"
)

// execBackend runs the system's ssh binary, which is how Forge has always
// reached its servers and remains the default.
//
// Its whole argument is that it is not ours: keys, ~/.ssh/config, known_hosts,
// agent forwarding, a hardware token, whatever the user has already made work —
// all of it applies without Forge knowing any of it exists. The pure-Go client
// buys the platforms where there is no binary to run; it does not buy this.
type execBackend struct{}

func (execBackend) Name() string { return "ssh" }

func (execBackend) Run(t Target, c Command) error {
	cmd := exec.Command("ssh", t.Args(c.Remote...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	return asExitError(cmd.Run())
}

// Open runs ssh under a pty on this machine, which is how every terminal in the
// UI has always been opened.
//
// The pty is the whole trick: ssh asks the server for a terminal because it
// believes it has one itself, and a window change written to this pty arrives at
// the far end as a SIGWINCH — none of which Forge has to implement, because
// OpenSSH does it. What it costs is a process per terminal, and a platform that
// can start one.
//
// The environment is inherited, deliberately: ssh sends this process's own TERM
// to the server, and everything the user has configured — ~/.ssh/config, an
// agent, a hardware token — applies exactly as it does for the commands above.
func (execBackend) Open(t Target, s Shell) (Terminal, error) {
	cols, rows := s.size()
	// Unwrapped rather than returned as a pair: a *localpty.Term that is nil
	// alongside an error would still be a non-nil Terminal to whoever ignored the
	// error.
	term, err := localpty.Start(exec.Command("ssh", t.ttyArgs(s)...), cols, rows)
	if err != nil {
		return nil, err
	}
	return term, nil
}

// asExitError turns a process that exited non-zero into the transport's own
// ExitError, so an operation reading a remote command's status does not have to
// know which backend ran it.
//
// Note what ssh's own failures look like here: an unreachable host or a refused
// key exits 255, so they arrive as an ExitError too. That is ssh's convention,
// not something this wrapper invents — see ExitError.
func asExitError(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode(), Err: err}
	}
	return err
}
