package forge

import (
	"bytes"
	"errors"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/internal/sshx"
)

// The workspace file browser. It is read-only and rooted at the workspace user's
// home (/home/workspaces/<name>): Claude edits, the human inspects. Paths are
// always relative to that home and cannot escape it — not for security (the
// caller is on your own machine, and this reads) but for orientation, so the tree
// you see and the session you see are always the same workspace.

// DirEntry is one child of a directory.
type DirEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// DirListing is one directory's immediate children, with the cleaned path they
// are relative to — the caller asked with whatever it had, and this is what that
// resolved to.
type DirListing struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
}

// FileText is as much of a file as a viewer gets: text, and whether there was
// more of it.
type FileText struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// What a read can fail as. The tree a caller browses is explicitly allowed to be
// stale — that is what its refresh button is for — so clicking something that has
// since been deleted, or replaced by a directory, is a normal path and must come
// back as itself rather than as "exit status 1". Each one is a sentinel so the
// caller can say it in its own words.
var (
	// ErrBadPath means the path was empty or reached outside the workspace home.
	ErrBadPath = errors.New("path is not inside the workspace home")
	// ErrNoSuchPath means nothing is there any more.
	ErrNoSuchPath = errors.New("no such path")
	ErrNotAFile   = errors.New("not a regular file")
	ErrNotADir    = errors.New("not a directory")
	// ErrNoHome means the workspace home could not be entered at all — the
	// workspace answers, but as something without a home to browse.
	ErrNoHome = errors.New("cannot reach the workspace home")
	// ErrBinaryFile means the file is not text, so there is nothing to show.
	ErrBinaryFile = errors.New("not a text file")
)

// maxFileBytes caps how much of a file is shipped to a viewer. This is an
// inspector, not an editor, so a couple of MB is plenty and keeps a huge log from
// hanging whoever opened it.
const maxFileBytes = 2_000_000

// ListDir returns the immediate children of a directory in the workspace,
// relative to the workspace home. Directories sort first, then names,
// case-insensitively. An empty path is the home itself.
func ListDir(workspace, dir string) (DirListing, error) {
	target, err := workspaceTarget(workspace)
	if err != nil {
		return DirListing{}, err
	}
	rel, ok := cleanRel(dir)
	if !ok {
		return DirListing{}, ErrBadPath
	}
	arg := rel
	if arg == "" {
		arg = "."
	}
	// find over one level; %y is the type (d/f/l…), %f the bare name. guardPath
	// gives a vanished or replaced directory its own exit code.
	remote := guardPath(arg, "-d") +
		`find -- "$p" -mindepth 1 -maxdepth 1 -printf '%y\t%f\n' 2>/dev/null`
	out, err := sshx.Capture(target.Args(remote)...)
	if err != nil {
		return DirListing{}, fsErr(err)
	}

	entries := []DirEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		typ, name, found := strings.Cut(line, "\t")
		if !found || name == "" {
			continue
		}
		entries = append(entries, DirEntry{Name: name, Dir: typ == "d"})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir // dirs first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	return DirListing{Path: rel, Entries: entries}, nil
}

// ReadFile returns up to maxFileBytes of a file's text, and whether it stopped
// there. The workspace home itself is not a file, so an empty path is ErrBadPath.
func ReadFile(workspace, file string) (FileText, error) {
	target, err := workspaceTarget(workspace)
	if err != nil {
		return FileText{}, err
	}
	rel, ok := cleanRel(file)
	if !ok || rel == "" {
		return FileText{}, ErrBadPath
	}
	// Read one byte past the cap so we can tell "exactly the cap" from "truncated".
	// Every literal here comes from a const, so shell and Go can't drift apart.
	remote := guardPath(rel, "-f") +
		`head -c ` + strconv.Itoa(maxFileBytes+1) + ` -- "$p"`
	out, err := sshx.Capture(target.Args(remote)...)
	if err != nil {
		return FileText{}, fsErr(err)
	}
	truncated := len(out) > maxFileBytes
	if truncated {
		out = out[:maxFileBytes]
	}
	// A NUL byte means this isn't text. Say so, rather than shipping mojibake to a
	// viewer that can only render text anyway.
	if bytes.IndexByte(out, 0) >= 0 {
		return FileText{}, ErrBinaryFile
	}
	// Truncation can slice a multi-byte rune in half, and an invalid UTF-8 tail
	// would be mangled on the way through JSON — drop it.
	return FileText{
		Path:      rel,
		Content:   strings.ToValidUTF8(string(out), ""),
		Truncated: truncated,
	}, nil
}

// Exit codes the remote snippets use to tell us *why* they failed, so a stale
// path produces a real answer rather than a bare failure.
const (
	rcNoHome   = 4
	rcNotFound = 5
	rcNotAFile = 6
	rcNotADir  = 7
)

// guardPath is the prelude of every remote fs command: enter the workspace home,
// then assert the target is there and of the expected type, each failure with its
// own exit code. Built from the rc* constants so the shell and Go can't drift
// apart. want is "-d" for a directory or "-f" for a regular file.
func guardPath(rel, want string) string {
	missRC := rcNotAFile
	if want == "-d" {
		missRC = rcNotADir
	}
	return `cd -- "$HOME" 2>/dev/null || exit ` + strconv.Itoa(rcNoHome) +
		`; p=` + shQuote(rel) +
		`; [ -e "$p" ] || exit ` + strconv.Itoa(rcNotFound) +
		`; [ ` + want + ` "$p" ] || exit ` + strconv.Itoa(missRC) + `; `
}

// fsErr turns a failed remote command into the sentinel its exit code stands for,
// leaving anything that wasn't an exit status (ssh itself failing) as it is.
func fsErr(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	switch ee.ExitCode() {
	case rcNoHome:
		return ErrNoHome
	case rcNotFound:
		return ErrNoSuchPath
	case rcNotAFile:
		return ErrNotAFile
	case rcNotADir:
		return ErrNotADir
	}
	return err
}

// cleanRel normalises a caller-supplied path into a clean workspace-relative
// path, rejecting anything that would escape the home directory. Empty means the
// root. ok is false for escapes.
func cleanRel(p string) (rel string, ok bool) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return "", true
	}
	c := path.Clean(p)
	if c == "." {
		return "", true
	}
	if c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return "", false
	}
	return c, true
}

// shQuote single-quotes a string for safe embedding in a remote shell command, so
// a filename with spaces or metacharacters can't break out.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
