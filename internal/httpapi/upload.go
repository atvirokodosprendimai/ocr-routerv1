package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// reservedParams are query keys that mean something to the router itself and
// must not leak through into the job's subprocess parameters.
var reservedParams = map[string]struct{}{
	"label":    {},
	"pipeline": {},
}

// handleUpload serves both roles on one path.
//
// ⚠ IT BRANCHES ON THE PRINCIPAL'S ROLE, NEVER ON THE BODY OR A HEADER. Letting
// the payload choose the branch is the role-confusion bug: a client could then
// post a worker-shaped JSON result and complete its own job without any work
// being done — and be charged for output it wrote itself.
func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	switch principal(r).Role {
	case core.RoleClient:
		a.uploadFromClient(w, r)
	case core.RoleWorker:
		a.resultFromWorker(w, r)
	default:
		writeError(w, core.ErrForbidden)
	}
}

type uploadResponse struct {
	JobID string `json:"job_id"`
}

// uploadFromClient accepts a new job.
//
// The body is OPTIONAL: a request with no multipart part at all is the crawler
// shape, where the job's input is its parameters and the service fetches its
// own source.
func (a *API) uploadFromClient(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()

	in := router.UploadInput{
		Params: map[string]string{},
	}

	// Pipeline wins over label when both are given: it is the more specific
	// statement of intent.
	switch {
	case q.Get("pipeline") != "":
		for _, l := range strings.Split(q.Get("pipeline"), ",") {
			if l = strings.TrimSpace(l); l != "" {
				in.Pipeline = append(in.Pipeline, l)
			}
		}
	case q.Get("label") != "":
		in.Pipeline = []string{q.Get("label")}
	}

	for k, vs := range q {
		if _, reserved := reservedParams[k]; reserved || len(vs) == 0 {
			continue
		}
		// Only the first value: a repeated key is ambiguous as a single flag,
		// and silently picking one of several is worse than picking the first
		// predictably.
		in.Params[k] = vs[0]
	}

	switch {
	case strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/"):
		r.Body = http.MaxBytesReader(w, r.Body, a.deps.MaxUpload)
		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, core.ErrInvalidParam)
			return
		}
		defer file.Close()
		in.Body = file
		in.Filename = header.Filename

	case r.ContentLength > 0:
		// A body that is NOT multipart is a mistake, and refusing it is the
		// point: the params-only crawler shape has no body at all, so "no body"
		// and "an unrecognised body" are different requests and must not be
		// collapsed. Accepting this would silently create an empty job for a
		// confused client — or for a worker that reached here with the wrong
		// token — and the caller would have no idea anything was wrong.
		writeError(w, core.ErrInvalidParam)
		return
	}

	job, err := a.deps.Router.Upload(r.Context(), p.UserID, in, a.deps.Now())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadResponse{JobID: job.ID})
}

// workerResult is what a worker posts back.
//
// Exactly one of Units or Error is meaningful. Both empty is treated as a
// zero-unit success, which is a legitimate outcome — a service can correctly
// produce nothing.
type workerResult struct {
	JobID string   `json:"job_id"`
	Units []string `json:"units"`
	Error string   `json:"error"`
}

// resultFromWorker records a stage's outcome.
func (a *API) resultFromWorker(w http.ResponseWriter, r *http.Request) {
	p := principal(r)

	var body workerResult
	if err := json.NewDecoder(io.LimitReader(r.Body, a.deps.MaxUpload)).Decode(&body); err != nil {
		writeError(w, core.ErrInvalidParam)
		return
	}
	if body.JobID == "" {
		writeError(w, core.ErrInvalidParam)
		return
	}

	// The worker's TOKEN id is the lease holder, not a name it chooses. A worker
	// naming its own id could complete a job leased to a different one.
	var err error
	if body.Error != "" {
		err = a.deps.Router.Fail(r.Context(), p.TokenID, body.JobID, body.Error, a.deps.Now())
	} else {
		err = a.deps.Router.Complete(r.Context(), p.TokenID, body.JobID, body.Units, a.deps.Now())
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
