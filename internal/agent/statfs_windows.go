//go:build windows

package agent

import "errors"

// statfs is unavailable on Windows; the agent never runs there (it manages Linux
// users), this exists only so the package compiles.
func statfs(string) (bsize, blocks, bfree uint64, err error) {
	return 0, 0, 0, errors.New("statfs: not supported on windows")
}
