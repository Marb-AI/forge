package localpty

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A pty is a local resource, and after the terminals moved behind the transport
// seam there is exactly one package left that needs one: this one. Both of its
// callers are the two places a terminal still runs on this machine — the login
// shell, and the ssh binary the default backend execs.
//
// The rule is written down because the compiler has no opinion on it, and because
// breaking it is how the seam stops being one: a package that starts its own
// process behind its own pty has decided that Forge runs where processes can be
// started, which is the assumption this whole initiative exists to remove.
func TestOnlyThisPackageKnowsWhatAPtyIs(t *testing.T) {
	root := filepath.Join("..", "..")
	var users []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "dist" || d.Name() == "bin") {
			return fs.SkipDir
		}
		// Test files are exempt: two of them name this dependency in order to
		// forbid it (see the front ends' own boundary tests), and a rule that
		// tripped over the tests that agree with it would be unreadable.
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("creack/pty")) {
			rel, _ := filepath.Rel(root, path)
			users = append(users, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) == 0 {
		t.Fatal("nothing imports a pty at all — this check has stopped matching what it was written for")
	}
	for _, f := range users {
		if strings.HasPrefix(f, "internal/localpty/") {
			continue
		}
		t.Errorf("%s starts something behind a pty of its own; a terminal comes from "+
			"the core (forge.OpenTerminal), and a remote one has its pty on the server", f)
	}
}

// The terminal has to actually be a terminal: a program that asks the kernel how
// big its window is must get the answer the caller gave, from the first byte —
// which is the whole reason the size is passed to Start rather than set after it.
func TestATerminalOpensAtTheSizeItWasGiven(t *testing.T) {
	term, err := Start(exec.Command("/bin/sh"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	// stty reads the size off the terminal it is attached to, so this is the child's
	// view rather than ours.
	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	if seen, ok := readUntil(term, "30 100", 10*time.Second); !ok {
		t.Errorf("the child does not see a 100x30 window; read so far:\n%s", seen)
	}
}

// And a resize has to reach it, because the browser's window is the real one.
func TestResizingTellsTheChild(t *testing.T) {
	term, err := Start(exec.Command("/bin/sh"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if err := term.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	if seen, ok := readUntil(term, "40 120", 10*time.Second); !ok {
		t.Errorf("the child still sees the old window; read so far:\n%s", seen)
	}
}

// Closing kills what was on the near end of the pty — for the exec'd backend that
// is the ssh process, and killing it is what makes closing a Claude terminal a
// detach rather than an exit.
func TestClosingKillsTheProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh")
	term, err := Start(cmd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if cmd.Process == nil {
		t.Fatal("nothing was started")
	}
	// Signal 0 asks "is this process still there" without sending anything. After a
	// Close that waited on it, it must not be.
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Error("the process outlived its terminal")
	}
}

// readUntil reads the terminal until marker shows up or the deadline passes,
// returning everything it read (for the failure message) and whether it found it.
func readUntil(term *Term, marker string, within time.Duration) (string, bool) {
	found := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			seen.Write(buf[:n])
			if strings.Contains(seen.String(), marker) {
				found <- seen.String()
				return
			}
			if err != nil {
				close(found)
				return
			}
		}
	}()
	select {
	case seen, ok := <-found:
		return seen, ok
	case <-time.After(within):
		return "", false
	}
}
