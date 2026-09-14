package httpapi

import (
	"errors"
	"net/http"
	"sort"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// claimResponse gives the worker everything it needs to build its argv without
// a second request.
type claimResponse struct {
	JobID    string            `json:"job_id"`
	Label    string            `json:"label"`
	Filename string            `json:"filename"`
	Params   map[string]string `json:"params"`
	HasBlob  bool              `json:"has_blob"`
	Stage    int               `json:"stage"`
	Stages   int               `json:"stages"`
}

// handleClaim leases one job for the label this worker serves.
//
// Worker-only, gated in the route table. The worker's TOKEN id is the lease
// holder — a worker does not get to name itself, or it could take a job leased
// to another.
func (a *API) handleClaim(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	label := r.URL.Query().Get("label")
	if label == "" {
		writeError(w, core.ErrInvalidParam)
		return
	}

	job, err := a.deps.Router.Claim(r.Context(), p.TokenID, label, a.deps.Now())
	if errors.Is(err, core.ErrNotFound) {
		// 204, not 404. An empty queue is the NORMAL answer to a polling
		// worker; returning an error status would fill the logs with failures
		// that are not failures and train the operator to ignore them.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, claimResponse{
		JobID:    job.ID,
		Label:    job.Label,
		Filename: job.Filename,
		Params:   job.Params,
		HasBlob:  job.HasBlob,
		Stage:    job.Stage,
		Stages:   len(job.Pipeline),
	})
}

type servicesResponse struct {
	Labels []string `json:"labels"`
}

// handleServices lists the services a client may currently ask for.
//
// The set is derived from live workers plus the grace window, so it answers "what
// can I send right now" rather than "what did an administrator once configure".
// A client that reads this cannot be surprised by an unknown-label refusal.
func (a *API) handleServices(w http.ResponseWriter, r *http.Request) {
	labels := a.deps.Router.AvailableLabels(a.deps.Now())
	sort.Strings(labels) // stable output; a set in map order is not a contract
	writeJSON(w, http.StatusOK, servicesResponse{Labels: labels})
}
