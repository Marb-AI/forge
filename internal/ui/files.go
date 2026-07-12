package ui

import (
	"bytes"
	"errors"
	"net/http"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/internal/sshx"
)

var (
	errBadPath  = errors.New("path escapes the workspace")
	errBinary   = errors.New("binary file — not shown")
	errNotFound = errors.New("no longer there — refresh the tree")
	errNotAFile = errors.New("not a regular file")
	errNotADir  = errors.New("not a directory")
	errNoHome   = errors.New("cannot reach the workspace home")
)

// Exit codes the remote snippets use to tell us *why* they failed. The tree is
// explicitly allowed to be stale (that's what its refresh button is for), so
// clicking something that has since been deleted is a normal path — it must
// produce a real message, not a bare "exit status 1".
const (
	rcNoHome   = 4
	rcNotFound = 5
	rcNotAFile = 6
	rcNotADir  = 7
)

// remoteExit returns the remote command's exit status, or -1 if the failure
// wasn't an exit status at all (ssh itself failed).
func remoteExit(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// fsError maps a remote exit code onto an HTTP response.
func fsError(w http.ResponseWriter, err error) {
	switch remoteExit(err) {
	case rcNotFound:
		writeJSONError(w, http.StatusNotFound, errNotFound)
	case rcNotAFile:
		writeJSONError(w, http.StatusUnsupportedMediaType, errNotAFile)
	case rcNotADir:
		writeJSONError(w, http.StatusUnsupportedMediaType, errNotADir)
	case rcNoHome:
		writeJSONError(w, http.StatusBadGateway, errNoHome)
	default:
		writeJSONError(w, http.StatusBadGateway, err)
	}
}

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

// The file browser is read-only and rooted at the workspace user's home
// (/home/workspaces/<name>): Claude edits, the human inspects. Paths are always
// relative to that home and can't escape it — not for security (it's localhost,
// read-only) but for orientation, so the tree you see and the Claude session you
// see are always the same workspace.

// maxFileBytes caps how much of a file we ship to the viewer. It's an inspector,
// not an editor, so a couple of MB is plenty and keeps a huge log from hanging.
const maxFileBytes = 2_000_000

type fsEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// handleFsList returns the immediate children of a directory (relative to the
// workspace home). Directories sort first, then names, case-insensitively.
func (s *server) handleFsList(w http.ResponseWriter, r *http.Request) {
	target, err := s.wsTarget(r.PathValue("ws"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	rel, ok := cleanRel(r.URL.Query().Get("path"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, errBadPath)
		return
	}
	arg := rel
	if arg == "" {
		arg = "."
	}
	// find over one level; %y is the type (d/f/l…), %f the bare name. guardPath
	// gives a vanished or replaced directory its own exit code (see fsError).
	remote := guardPath(arg, "-d") +
		`find -- "$p" -mindepth 1 -maxdepth 1 -printf '%y\t%f\n' 2>/dev/null`
	out, err := sshx.Capture(target.Args(remote)...)
	if err != nil {
		fsError(w, err)
		return
	}

	entries := []fsEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		typ, name, found := strings.Cut(line, "\t")
		if !found || name == "" {
			continue
		}
		entries = append(entries, fsEntry{Name: name, Dir: typ == "d"})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir // dirs first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	writeJSON(w, map[string]any{"path": rel, "entries": entries})
}

// handleFsRead returns up to maxFileBytes of a file's text for the viewer.
func (s *server) handleFsRead(w http.ResponseWriter, r *http.Request) {
	target, err := s.wsTarget(r.PathValue("ws"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	rel, ok := cleanRel(r.URL.Query().Get("path"))
	if !ok || rel == "" {
		writeJSONError(w, http.StatusBadRequest, errBadPath)
		return
	}
	// Read one byte past the cap so we can tell "exactly the cap" from "truncated".
	// Every literal here comes from a const, so shell and Go can't drift apart.
	remote := guardPath(rel, "-f") +
		`head -c ` + strconv.Itoa(maxFileBytes+1) + ` -- "$p"`
	out, err := sshx.Capture(target.Args(remote)...)
	if err != nil {
		fsError(w, err)
		return
	}
	truncated := len(out) > maxFileBytes
	if truncated {
		out = out[:maxFileBytes]
	}
	// A NUL byte means this isn't text. Say so, rather than shipping mojibake to
	// a viewer that can only render text anyway.
	if bytes.IndexByte(out, 0) >= 0 {
		writeJSONError(w, http.StatusUnsupportedMediaType, errBinary)
		return
	}
	// Truncation can slice a multi-byte rune in half, and an invalid UTF-8 tail
	// would be mangled on the way through JSON — drop it.
	content := strings.ToValidUTF8(string(out), "")

	writeJSON(w, map[string]any{"path": rel, "content": content, "truncated": truncated})
}

// cleanRel normalises a browser-supplied path into a clean workspace-relative
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

// shQuote single-quotes a string for safe embedding in a remote shell command,
// so a filename with spaces or metacharacters can't break out.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
