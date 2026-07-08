//go:build windows

package proc

import "syscall"

const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	queryLimitedInfo      = 0x1000
	stillActive           = 259
)

// DetachAttr detaches a spawned process so it outlives the launching shell.
func DetachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
}

// ChildAttr puts a child (an ssh tunnel) in its own process group.
func ChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// Alive reports whether a process with the given pid currently exists.
func Alive(pid int) bool {
	h, err := syscall.OpenProcess(queryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true // handle opened but can't query — assume alive
	}
	return code == stillActive
}

// Terminate stops the process. Windows has no SIGTERM, so this is a hard kill;
// the supervisor cannot clean up gracefully (see docs). Callers treat a stale
// pidfile as not-running, so this is safe for lifecycle purposes.
func Terminate(pid int) error {
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	return syscall.TerminateProcess(h, 1)
}
