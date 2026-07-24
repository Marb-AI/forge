//go:build !windows

package agent

import "syscall"

// statfs reports a filesystem's block size, its total blocks and the blocks
// still free, straight from the kernel.
//
// It is split by platform for the same reason internal/proc is: the agent only
// ever RUNS on Linux, but the package still has to COMPILE wherever `go build
// ./...` is run, and Windows has no statfs at all. Bsize is int64 on Linux and
// uint32 on macOS, so it is converted rather than returned as-is.
func statfs(path string) (bsize, blocks, bfree uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	return uint64(st.Bsize), st.Blocks, st.Bfree, nil
}
