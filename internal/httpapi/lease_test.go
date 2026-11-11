package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// claimLease uploads a units job, claims it, and returns its id and the lease
// the claim answer carried.
func (e *env) claimLease(t *testing.T, label string) (id, lease string) {
	t.Helper()
	e.liveWorker(t, label)
	e.unitsService(t, label, 1)
	body, ct := multipartBody(t, "in.pdf", "source")
	if resp := e.do(t, "POST", "/upload?label="+label, e.clientTok, body, ct); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	c := e.do(t, "POST", "/claim?label="+label+"&raw=0", e.workerTok, nil, "")
	if c.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", c.StatusCode)
	}
	var got struct {
		JobID string `json:"job_id"`
		Lease string `json:"lease"`
	}
	decodeJSON(t, c, &got)
	return got.JobID, got.Lease
}

// TestClaimAnswerCarriesTheLease: the token a worker must present is the one
// the claim stored (ADR-0013).
func TestClaimAnswerCarriesTheLease(t *testing.T) {
	e := newEnv(t)
	id, lease := e.claimLease(t, "ocr")
	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if lease == "" || lease != job.LeaseToken {
		t.Errorf("claim answer lease = %q, row lease_token = %q: want the same non-empty token", lease, job.LeaseToken)
	}
}

// TestWorkerRoutesRefuseAWrongLease: each route a worker names a job on reads
// the lease beside the job id and refuses one that is not the current
// claim's with 409, leaving the job with its holder; the right one works.
func TestWorkerRoutesRefuseAWrongLease(t *testing.T) {
	routes := map[string]func(e *env, id, lease string) *http.Response{
		"result": func(e *env, id, lease string) *http.Response {
			b := []byte(`{"job_id":"` + id + `","lease":"` + lease + `","units":["p"]}`)
			return e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader(b), "application/json")
		},
		"failure": func(e *env, id, lease string) *http.Response {
			b := []byte(`{"job_id":"` + id + `","lease":"` + lease + `","error":"boom"}`)
			return e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader(b), "application/json")
		},
		"input": func(e *env, id, lease string) *http.Response {
			return e.do(t, "GET", "/files/"+id+"?lease="+lease, e.workerTok, nil, "")
		},
		"release": func(e *env, id, lease string) *http.Response {
			return e.do(t, "POST", "/release?job_id="+id+"&lease="+lease, e.workerTok, nil, "")
		},
	}
	for name, call := range routes {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id, lease := e.claimLease(t, "ocr")

			if resp := call(e, id, "not-"+lease); resp.StatusCode != http.StatusConflict {
				t.Errorf("%s with a wrong lease = %d, want 409", name, resp.StatusCode)
			}
			job, _ := e.repo.JobByID(context.Background(), id)
			if job.State != core.JobProcessing || job.LeaseToken != lease {
				t.Errorf("after the refused %s the job is %q under %q, want processing under the claim",
					name, job.State, job.LeaseToken)
			}
			if resp := call(e, id, lease); resp.StatusCode/100 != 2 {
				t.Errorf("%s with the claim's lease = %d, want 2xx", name, resp.StatusCode)
			}
		})
	}
}
