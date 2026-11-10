package httpapi

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// handleFile serves both roles: a client collects its result, a worker fetches
// the source it holds the lease on.
func (a *API) handleFile(w http.ResponseWriter, r *http.Request) {
	switch principal(r).Role {
	case core.RoleClient:
		a.resultToClient(w, r)
	case core.RoleWorker:
		a.blobToWorker(w, r)
	default:
		writeError(w, core.ErrForbidden)
	}
}

type resultResponse struct {
	JobID string   `json:"job_id"`
	Units []string `json:"units"`
}

// resultToClient delivers and charges.
//
// This GET mutates, which is the one deliberate exception in the API, and it is
// bounded: delivery is idempotent TO THE HOLDER, because a second call on a
// units job finds no result and is a 404 rather than a second charge. A
// separate acknowledge endpoint was considered and is noted in the task's Stop
// Condition; the cost of this shape is that a client whose connection drops
// mid-download has been charged for bytes it did not finish reading. For a RAW
// result that cost is bounded: the file stays for a result TTL after the charge
// and a retry streams it again uncharged (F3).
func (a *API) resultToClient(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := chi.URLParam(r, "id")

	// A raw job's result is a FILE, so it is streamed rather than rendered. The
	// client branches on the response Content-Type, never on what it asked for:
	// the two disagree exactly when something is wrong, and that is the case
	// worth reporting instead of misreading.
	job, err := a.deps.Repo.JobByID(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if job.Raw {
		f, err := a.deps.Router.DeliverRaw(r.Context(), p.UserID, id, a.deps.Now())
		if err != nil {
			writeError(w, err)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, f); err != nil {
			// The charge has committed and the bytes are partly sent; there is no
			// status left to change. Leave the blob: DeliverRaw serves a
			// delivered job's file again without a charge, and the reaper
			// removes it past the result TTL (F3).
			return
		}
		a.deps.Router.DropRawResult(id)
		return
	}

	res, err := a.deps.Router.Deliver(r.Context(), p.UserID, id, a.deps.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resultResponse{JobID: res.JobID, Units: res.Units})
}

// blobToWorker streams the source file to the worker holding the lease.
func (a *API) blobToWorker(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := chi.URLParam(r, "id")

	job, err := a.deps.Repo.JobByID(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	// The lease is the authorisation. Without this a worker could read any
	// customer's source file by guessing — or simply enumerating — job ids.
	if job.WorkerID != p.TokenID || job.State != core.JobProcessing {
		writeError(w, core.ErrConflict)
		return
	}
	if !job.HasBlob {
		// A params-only job has nothing to download, and saying so plainly is
		// better than serving an empty body the worker would treat as a file.
		writeError(w, core.ErrNotFound)
		return
	}

	f, err := a.deps.Blobs.Open(id)
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
