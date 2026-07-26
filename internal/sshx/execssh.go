package sshx

import (
	"errors"
	"os/exec"
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
