package httpapi_test

import (
	"context"
	"net/http"
	"testing"
)

// TestClientRawOnUnitsLabelIsRefused is the client half of ADR-0006's three-way
// mode agreement.
func TestClientRawOnUnitsLabelIsRefused(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	e.unitsService(t, "ocr", 3)

	body, ct := multipartBody(t, "doc.pdf", "hello")
	resp := e.do(t, "POST", "/upload?label=ocr&raw=1", e.clientTok, body, ct)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("raw upload to a units label = %d, want 409", resp.StatusCode)
	}

	// And nothing was admitted: a refusal that still created a row would leave a
	// job nobody can price.
	jobs, err := e.repo.ListJobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("a refused upload created %d job(s)", len(jobs))
	}
}

// TestClientUnitsOnRawLabelIsRefused pins the direction that protects the
// CLIENT: an un-upgraded caller must not silently receive bytes it will treat as
// text. Absent and raw=0 are the same request, and both are refused here.
func TestClientUnitsOnRawLabelIsRefused(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "convert")
	e.rawService(t, "convert", 1)

	for _, q := range []string{"/upload?label=convert", "/upload?label=convert&raw=0"} {
		body, ct := multipartBody(t, "doc.pdf", "hello")
		resp := e.do(t, "POST", q, e.clientTok, body, ct)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s = %d, want 409 — a client that did not ask for raw must not be handed bytes",
				q, resp.StatusCode)
		}
	}
}

// TestRawIsNotPassedToArgv is a security assertion, not bookkeeping.
//
// Params become subprocess FLAGS on the worker (ADR-0001), so a router-reserved
// word that leaks through arrives as `-raw` on someone's command line.
func TestRawIsNotPassedToArgv(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "convert")
	e.rawService(t, "convert", 1)

	body, ct := multipartBody(t, "doc.pdf", "hello")
	resp := e.do(t, "POST", "/upload?label=convert&raw=1&lang=lit", e.clientTok, body, ct)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var got struct {
		JobID string `json:"job_id"`
	}
	decodeJSON(t, resp, &got)

	job, err := e.repo.JobByID(context.Background(), got.JobID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if _, leaked := job.Params["raw"]; leaked {
		t.Error("`raw` reached the job's params — it would become a subprocess flag")
	}
	if job.Params["lang"] != "lit" {
		t.Errorf("an ordinary param was lost: params = %v", job.Params)
	}
	if !job.Raw {
		t.Error("the job was not stamped raw despite being admitted to a raw service")
	}
}

// TestRawRefusalReadsNoBody re-asserts the streaming property established in
// b84f45a against the NEW refusal path.
//
// A mode mismatch is decided from the query and the admin record alone, so it
// must cost nothing — the same guarantee the credit and buffer refusals have.
func TestRawRefusalReadsNoBody(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	e.unitsService(t, "ocr", 3)

	var read int64
	handlerDone := make(chan int64, 1)
	srv := newCountingServer(t, e, &read, handlerDone)

	const size = 1 << 20
	body, ct := multipartBody(t, "big.pdf", repeat("x", size))
	req, err := http.NewRequest("POST", srv.URL+"/upload?label=ocr&raw=1", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.clientTok)
	req.Header.Set("Content-Type", ct)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("mode mismatch = %d, want 409", resp.StatusCode)
	}
	if got := <-handlerDone; got > size/8 {
		t.Errorf("handler read %d of %d body bytes before refusing a mode mismatch — the refusal "+
			"is decided from the query and the admin record, so it must cost nothing", got, size)
	}
}
