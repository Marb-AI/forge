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
	// Pointers, not plain bools. A missing JSON field decodes to false, which for
	// these three would mean "no firewall, no SSH hardening, no clean-up" — the
	// opposite of what the CLI does. A silently unhardened server is not something
	// a forgotten field should be able to produce, so absent means the safe
	// default: on.
	var req struct {
		Target      string  `json:"target"`
		Alias       string  `json:"alias"`
		Jump        *string `json:"jump"`
		Firewall    *bool   `json:"firewall"`
		Harden      *bool   `json:"harden"`
		DockerPrune *bool   `json:"dockerPrune"`
		PruneImages *bool   `json:"pruneImages"`
		// Opt-in like PruneImages, and for a stronger reason: it widens the volume
		// pass to NAMED volumes, which is where a `compose down`-ed stack's data is.
		PruneVolumes *bool `json:"pruneVolumes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("bad request"))
		return
	}
	req.Target, req.Alias = strings.TrimSpace(req.Target), strings.TrimSpace(req.Alias)
	// A pointer for the same reason the three below are: absent is not the same
	// answer as empty. Re-preparing a host without saying anything about its route
	// keeps the one on record; sending "" clears it.
	if req.Jump != nil {
		trimmed := strings.TrimSpace(*req.Jump)
		req.Jump = &trimmed
	}
	if req.Target == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("ssh target required, e.g. root@1.2.3.4"))
		return
	}
	if !validName(req.Alias) {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("alias must be 1-32 chars: letters, digits, dash or underscore"))
		return
	}

	firewall, harden, prune := onByDefault(req.Firewall), onByDefault(req.Harden), onByDefault(req.DockerPrune)
	// Unlike the three above, the aggressive image sweep is opt-in: absent (or
	// false) means off, so a forgotten field never deletes a workspace's images.
	pruneImages := req.PruneImages != nil && *req.PruneImages
	pruneVolumes := req.PruneVolumes != nil && *req.PruneVolumes
	// They're tiers of the nightly clean-up, injected into that script — so asking
	// for one while declining the clean-up would install nothing. Reject the
	// combination instead of silently doing nothing (the wizard also disables them
	// in that case).
	if pruneImages && !prune {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("pruneImages needs the nightly clean-up: don't combine it with dockerPrune:false"))
		return
	}
	if pruneVolumes && !prune {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Errorf("pruneVolumes needs the nightly clean-up: don't combine it with dockerPrune:false"))
		return
	}

	id, err := s.startJob(func(out io.Writer) error {
		return s.deps.PrepareHost(req.Target, req.Alias, req.Jump, firewall, harden, prune, pruneImages, pruneVolumes, out)
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"id": id})
}

// onByDefault reads an optional JSON bool: absent means on. These guard a real
// machine, so forgetting to send one must not turn it off.
func onByDefault(b *bool) bool { return b == nil || *b }
