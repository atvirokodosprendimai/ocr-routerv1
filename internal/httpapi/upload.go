package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// filePart returns the "file" part of a multipart request as a STREAM, without
// parsing the form.
//
// r.FormFile is shorter and is the wrong tool here. It parses the WHOLE form up
// front: up to 32 MiB of the file is held in memory and the remainder is spilled
// to a temp file in os.TempDir, so every byte is written to disk twice — once by
// the form parser and again by blob.Put — and the entire upload is absorbed
// before Upload is even reached, which means a client with no credits, at its
// buffer limit, or naming a label no worker serves still costs the router a full
// copy of the file before being refused.
//
// Handing the part itself to Upload lets blob.Put copy the network straight into
// the blob's staging file, and lets every refusal cost nothing.
//
// A part with no filename is skipped for the same reason FormFile ignores one:
// it is an ordinary form field, not the source file.
func filePart(r *http.Request) (*multipart.Part, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	for {
		// io.EOF here means the body held no "file" part at all, which is an
		// error rather than an empty upload: the params-only crawler shape sends
		// no body, not an empty multipart one.
		p, err := mr.NextPart()
		if err != nil {
			return nil, err
		}
		if p.FormName() == "file" && p.FileName() != "" {
			return p, nil
		}
		_ = p.Close()
	}
}

// reservedParams are query keys that mean something to the router itself and
// must not leak through into the job's subprocess parameters.
var reservedParams = map[string]struct{}{
	"label":    {},
	"pipeline": {},
	// `raw` is the client's declared output mode. Reserving it is the security
	// half: a param that is NOT reserved becomes a subprocess FLAG on the worker
	// (ADR-0001), so leaking this one puts `-raw` on somebody's command line.
	"raw": {},
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

	raw, err := rawParam(r)
	if err != nil {
		writeError(w, err)
		return
	}
	in := router.UploadInput{
		Params: map[string]string{},
		Raw:    raw,
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
		part, err := filePart(r)
		if err != nil {
			writeError(w, core.ErrInvalidParam)
			return
		}
		// The part is deliberately NOT closed. multipart.Part.Close drains
		// whatever is left of the part into io.Discard so the next part can be
		// read, and there is no next part here — so on a refusal it would pull
		// the entire file off the wire for nothing, which is most of what this
		// handler was changed to stop doing. The part is backed by r.Body, and
		// net/http closes that.
		in.Body = part
		in.Filename = part.FileName()

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
		// The cap is enforced by MaxBytesReader while blob.Put streams the part,
		// so an oversize upload now surfaces as a blob write failure. Without
		// this it would map to 500 — a server fault — when it is the client that
		// sent too much.
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = fmt.Errorf("%w: upload exceeds the %d byte limit", core.ErrInvalidParam, tooBig.Limit)
		}
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
//
// ⚠ IT BRANCHES ON Content-Type, AND THAT IS NOT THE ROLE-CONFUSION BUG. The
// role branch in handleUpload is unchanged and still first; this selects a
// PAYLOAD SHAPE after the principal's role is already settled. The body cannot
// choose the mode either, because jobs.raw was stamped at admission and
// CompleteRaw/Complete each refuse a job of the other kind — so a worker sending
// bytes for a units job is refused, and vice versa.
func (a *API) resultFromWorker(w http.ResponseWriter, r *http.Request) {
	p := principal(r)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/octet-stream") {
		jobID := r.URL.Query().Get("job_id")
		if jobID == "" {
			writeError(w, core.ErrInvalidParam)
			return
		}
		// The id rides the query because the body IS the payload and has no room
		// for an envelope. Streamed under the same cap as a client upload.
		r.Body = http.MaxBytesReader(w, r.Body, a.deps.MaxUpload)
		if err := a.deps.Router.CompleteRaw(r.Context(), p.TokenID, jobID, r.Body, a.deps.Now()); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				err = fmt.Errorf("%w: result exceeds the %d byte limit", core.ErrInvalidParam, tooBig.Limit)
			}
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

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
