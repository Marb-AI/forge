package ui

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Marb-AI/forge/forge"
)

// The browser's end of the workspace file browser. Reading a workspace is the
// core's operation (forge.ListDir, forge.ReadFile); what is left here is the part
// that is HTTP's — which status a failure is, and what to say about it in a
// browser that is showing a tree.

// The types the browser reads are the core's own, as everywhere else here.
type (
	// DirEntry is one child of a directory: a name, and whether it is a directory.
	DirEntry = forge.DirEntry
	// DirListing is one directory's children and the path they are under.
	DirListing = forge.DirListing
	// FileText is a file's text, and whether there was more of it.
	FileText = forge.FileText
)

var (
	errBadPath  = errors.New("path escapes the workspace")
	errBinary   = errors.New("binary file — not shown")
	errNotFound = errors.New("no longer there — refresh the tree")
	errNotAFile = errors.New("not a regular file")
	errNotADir  = errors.New("not a directory")
	errNoHome   = errors.New("cannot reach the workspace home")
)

// fsError answers a failed read. The tree the browser shows is explicitly allowed
// to be stale — that is what its refresh button is for — so a path that has since
// been deleted or replaced is a normal thing to click, and each way it can fail
// gets a status and a sentence a person can act on rather than a bare 502.
func fsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forge.ErrBadPath):
		writeJSONError(w, http.StatusBadRequest, errBadPath)
	case errors.Is(err, forge.ErrNoSuchPath):
		writeJSONError(w, http.StatusNotFound, errNotFound)
	case errors.Is(err, forge.ErrNotAFile):
		writeJSONError(w, http.StatusUnsupportedMediaType, errNotAFile)
	case errors.Is(err, forge.ErrNotADir):
		writeJSONError(w, http.StatusUnsupportedMediaType, errNotADir)
	case errors.Is(err, forge.ErrBinaryFile):
		writeJSONError(w, http.StatusUnsupportedMediaType, errBinary)
	case errors.Is(err, forge.ErrNoHome):
		writeJSONError(w, http.StatusBadGateway, errNoHome)
	default:
		writeJSONError(w, http.StatusBadGateway, err)
	}
}

// handleFsList returns the immediate children of a directory (relative to the
// workspace home).
func (s *server) handleFsList(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	listing, err := s.deps.ListDir(ws, r.URL.Query().Get("path"))
	if err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, listing)
}

// handleFsRead returns as much of a file's text as the viewer gets.
func (s *server) handleFsRead(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	if !s.deps.KnowsWorkspace(ws) {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("unknown workspace %q", ws))
		return
	}
	file, err := s.deps.ReadFile(ws, r.URL.Query().Get("path"))
	if err != nil {
		fsError(w, err)
		return
	}
	writeJSON(w, file)
}
