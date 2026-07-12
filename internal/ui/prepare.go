package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handlePrepareHost provisions a bare server and registers it. It runs as a job
// (see jobs.go): the browser follows the output at /api/jobs/{id}/stream, so the
// wizard shows the same provisioning run you'd watch in a terminal — including
// the SSH key it prints for GitHub.
func (s *server) handlePrepareHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target      string `json:"target"`
		Alias       string `json:"alias"`
		Firewall    bool   `json:"firewall"`
		Harden      bool   `json:"harden"`
		DockerPrune bool   `json:"dockerPrune"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("bad request"))
		return
	}
	req.Target, req.Alias = strings.TrimSpace(req.Target), strings.TrimSpace(req.Alias)
	if req.Target == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("ssh target required, e.g. root@1.2.3.4"))
		return
	}
	if !validName(req.Alias) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("alias must be 1-32 chars: letters, digits, dash or underscore"))
		return
	}

	id, err := s.startJob(func(out io.Writer) error {
		return s.deps.PrepareHost(req.Target, req.Alias, req.Firewall, req.Harden, req.DockerPrune, out)
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"id": id})
}
