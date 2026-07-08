//go:build !windows

// Package proc abstracts the few OS-specific process operations the forwarding
// supervisor needs (detaching, process-group isolation, liveness, termination),
// so the forge client builds and runs on Unix and Windows alike.
package proc

import "syscall"

// DetachAttr detaches a spawned process from the controlling terminal so it
// outlives the shell that started it.
func DetachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// ChildAttr puts a child (an ssh tunnel) in its own process group.
func ChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// Alive reports whether a process with the given pid currently exists.
func Alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// Terminate asks the process to shut down (SIGTERM, so it can clean up).
func Terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
