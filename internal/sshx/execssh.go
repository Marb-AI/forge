package sshx

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Marb-AI/forge/internal/localpty"
	"github.com/Marb-AI/forge/internal/proc"
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

// Forward starts `ssh -N -L` and hands back the process as a tunnel.
//
// The forward is the ssh process's, in every sense: it binds the local port, it
// carries the connections, and it holds them for exactly as long as it lives. So
// a tunnel here is a process to wait on and a process to kill, and everything
// that can go wrong with it is something ssh printed on its way out.
func (execBackend) Forward(t Target, local, remote int) (Tunnel, error) {
	cmd := exec.Command("ssh", t.LocalForwardArgs(local, remote)...)
	// Its own process group, so a signal sent to Forge's does not go to the
	// tunnels as well.
	cmd.SysProcAttr = proc.ChildAttr()
	tun := &execTunnel{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &tun.stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		tun.err = tun.classify(cmd.Wait())
		close(tun.done)
	}()
	return tun, nil
}

// execTunnel is a tunnel that is a running ssh process.
//
// The result is settled once, by the goroutine that waited on the process, and
// read by everyone afterwards — which is what lets Wait and Close both be called,
// in either order, from either side, and both mean what they say.
type execTunnel struct {
	cmd    *exec.Cmd
	stderr bytes.Buffer
	// done closes when the process has been reaped and err is final. Reaped is the
	// word that matters for Close: the local port is not free until then.
	done chan struct{}
	err  error
	kill sync.Once
	// closing is set before the process is signalled, so the exit that follows is
	// read as the shutdown it is rather than as a tunnel that died.
	closing atomic.Bool
}

func (t *execTunnel) Wait() error {
	<-t.done
	return t.err
}

func (t *execTunnel) Close() error {
	t.kill.Do(func() {
		t.closing.Store(true)
		if p := t.cmd.Process; p != nil {
			_ = p.Kill()
		}
	})
	<-t.done
	return nil
}

// classify turns the way ssh left into the tunnel's answer.
//
// Reading the stderr after Wait is safe and not incidental: os/exec copies a
// process's output into a buffer on a goroutine of its own and waits for that
// goroutine before returning, so everything the process said is here.
//
// A process this backend killed reports a signal, which is not a failure of the
// tunnel: the holder asked for it, and Wait says so with nil.
func (t *execTunnel) classify(waitErr error) error {
	if t.closing.Load() {
		return nil
	}
	msg := strings.TrimSpace(t.stderr.String())
	switch {
	case authFailed(msg):
		return fmt.Errorf("%w: %s", ErrAuth, firstLine(msg))
	case portBusy(msg):
		return fmt.Errorf("%w: %s", ErrPortBusy, firstLine(msg))
	}
	if line := firstLine(msg); line != "" {
		return errors.New(line)
	}
	return waitErr
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
