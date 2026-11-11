package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// claimLease uploads a job in the given mode, claims it, and returns its id
// and the lease the claim answer carried — never empty, or every "right
// lease" half below would pass through the token-less path instead.
func (e *env) claimLease(t *testing.T, label string, raw bool) (id, lease string) {
	t.Helper()
	e.liveWorker(t, label)
	mode := "0"
	if raw {
		e.rawService(t, label, 1)
		mode = "1"
	} else {
		e.unitsService(t, label, 1)
	}
	body, ct := multipartBody(t, "in.pdf", "source")
	if resp := e.do(t, "POST", "/upload?label="+label+"&raw="+mode, e.clientTok, body, ct); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	c := e.do(t, "POST", "/claim?label="+label+"&raw="+mode, e.workerTok, nil, "")
	if c.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", c.StatusCode)
	}
	var got struct {
		JobID string `json:"job_id"`
		Lease string `json:"lease"`
	}
	decodeJSON(t, c, &got)
	if got.Lease == "" {
		t.Fatal("the claim answer carried no lease")
	}
	return got.JobID, got.Lease
}

// TestClaimAnswerCarriesTheLease: the token a worker must present is the one
// the claim stored (ADR-0013).
func TestClaimAnswerCarriesTheLease(t *testing.T) {
	e := newEnv(t)
	id, lease := e.claimLease(t, "ocr", false)
	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if lease != job.LeaseToken {
		t.Errorf("claim answer lease = %q, row lease_token = %q: want the same token", lease, job.LeaseToken)
	}
}

// workerRoute is one worker request about a job, the mode of job it needs,
// and the status it answers on success.
type workerRoute struct {
	raw  bool
	ok   int
	call func(t *testing.T, e *env, id, lease string) *http.Response
}

// workerRoutes is every route a worker names a job on. The lease rides beside
// the job id; "" sends none (a worker built before ADR-0013).
var workerRoutes = map[string]workerRoute{
	"result": {ok: http.StatusNoContent, call: func(t *testing.T, e *env, id, lease string) *http.Response {
		b := []byte(`{"job_id":"` + id + `","lease":"` + lease + `","units":["p"]}`)
		return e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader(b), "application/json")
	}},
	"failure": {ok: http.StatusNoContent, call: func(t *testing.T, e *env, id, lease string) *http.Response {
		b := []byte(`{"job_id":"` + id + `","lease":"` + lease + `","error":"boom"}`)
		return e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader(b), "application/json")
	}},
	"raw result": {raw: true, ok: http.StatusNoContent, call: func(t *testing.T, e *env, id, lease string) *http.Response {
		return e.do(t, "POST", "/upload?job_id="+id+"&lease="+lease, e.workerTok,
			strings.NewReader("bytes"), "application/octet-stream")
	}},
	"input": {ok: http.StatusOK, call: func(t *testing.T, e *env, id, lease string) *http.Response {
		return e.do(t, "GET", "/files/"+id+"?lease="+lease, e.workerTok, nil, "")
	}},
	"release": {ok: http.StatusNoContent, call: func(t *testing.T, e *env, id, lease string) *http.Response {
		return e.do(t, "POST", "/release?job_id="+id+"&lease="+lease, e.workerTok, nil, "")
	}},
}

// TestWorkerRoutesRefuseAWrongLease: each route reads the lease beside the job
// id and refuses one that is not the current claim's with 409, leaving the job
// with its holder; the right one gets the route's own success status.
func TestWorkerRoutesRefuseAWrongLease(t *testing.T) {
	for name, rt := range workerRoutes {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id, lease := e.claimLease(t, "svc", rt.raw)

			if resp := rt.call(t, e, id, "not-"+lease); resp.StatusCode != http.StatusConflict {
				t.Errorf("%s with a wrong lease = %d, want 409", name, resp.StatusCode)
			}
			job, _ := e.repo.JobByID(context.Background(), id)
			if job.State != core.JobProcessing || job.LeaseToken != lease {
				t.Errorf("after the refused %s the job is %q under %q, want processing under the claim",
					name, job.State, job.LeaseToken)
			}
			if resp := rt.call(t, e, id, lease); resp.StatusCode != rt.ok {
				t.Errorf("%s with the claim's lease = %d, want %d", name, resp.StatusCode, rt.ok)
			}
		})
	}
}

// TestWorkerRoutesAcceptNoLeaseForThisRelease is the one-release promise of
// ADR-0013: a worker that sends no lease at all still works on every route.
func TestWorkerRoutesAcceptNoLeaseForThisRelease(t *testing.T) {
	for name, rt := range workerRoutes {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id, _ := e.claimLease(t, "svc", rt.raw)
			if resp := rt.call(t, e, id, ""); resp.StatusCode != rt.ok {
				t.Errorf("%s without a lease = %d, want %d for this release", name, resp.StatusCode, rt.ok)
			}
		})
	}
}
