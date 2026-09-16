package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// pngMagic is deliberately not valid UTF-8: 0x89 is a continuation byte with no
// lead, so encoding/json silently replaces it with U+FFFD. Every fixture on this
// path is a real magic number for that reason — text round-trips perfectly
// through the channel this ADR exists to replace.
const pngMagic = "\x89PNG\r\n\x1a\n"

// rawJobInFlight admits a raw job and leases it to the worker, returning its id.
func (e *env) rawJobInFlight(t *testing.T, label string) string {
	t.Helper()
	e.liveWorker(t, label)
	e.rawService(t, label, 1)

	body, ct := multipartBody(t, "in.bin", "source")
	resp := e.do(t, "POST", "/upload?label="+label+"&raw=1", e.clientTok, body, ct)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	decodeJSON(t, resp, &created)

	claim := e.do(t, "POST", "/claim?label="+label+"&raw=1", e.workerTok, nil, "")
	if claim.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", claim.StatusCode)
	}
	return created.JobID
}

// TestRawResultRoundTripsByteForByte is the end-to-end assertion this whole ADR
// exists for: the exact bytes a worker printed reach the client.
func TestRawResultRoundTripsByteForByte(t *testing.T) {
	e := newEnv(t)
	id := e.rawJobInFlight(t, "convert")

	post := e.do(t, "POST", "/upload?job_id="+id, e.workerTok,
		bytes.NewReader([]byte(pngMagic)), "application/octet-stream")
	if post.StatusCode != http.StatusNoContent {
		t.Fatalf("posting a raw result = %d, want 204", post.StatusCode)
	}

	got := e.do(t, "GET", "/files/"+id, e.clientTok, nil, "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("collecting = %d, want 200", got.StatusCode)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream — the client branches on it", ct)
	}
	out, _ := io.ReadAll(got.Body)
	if string(out) != pngMagic {
		t.Errorf("delivered % x (%d bytes), want % x (%d bytes)",
			out, len(out), pngMagic, len(pngMagic))
	}
}

// TestRawResultDeletedOnDelivery keeps the property the memory-only result store
// had: delivery is idempotent TO THE HOLDER, so a second call is a 404 rather
// than a second charge.
func TestRawResultDeletedOnDelivery(t *testing.T) {
	e := newEnv(t)
	id := e.rawJobInFlight(t, "convert")

	e.do(t, "POST", "/upload?job_id="+id, e.workerTok,
		bytes.NewReader([]byte(pngMagic)), "application/octet-stream")

	if first := e.do(t, "GET", "/files/"+id, e.clientTok, nil, ""); first.StatusCode != http.StatusOK {
		t.Fatalf("first collect = %d, want 200", first.StatusCode)
	}
	second := e.do(t, "GET", "/files/"+id, e.clientTok, nil, "")
	if second.StatusCode != http.StatusNotFound {
		t.Errorf("second collect = %d, want 404 — delivery must not be repeatable, or the "+
			"customer is charged twice for one job", second.StatusCode)
	}

	// ⚠ And the FILE is gone. The 404 above does not prove this: the job's
	// done→delivered transition is what makes a second collect fail, so the
	// status is already correct with the blob still on disk. A mutation showed
	// exactly that — removing the delete left every test green while every
	// delivered raw result stayed on disk forever, which is unbounded growth on
	// the success path, where no reaper sweep is looking.
	if n := countSuffix(t, e.blobs.Dir(), ".out"); n != 0 {
		t.Errorf("a DELIVERED raw job left %d result blob(s) on disk — delivery is the success "+
			"path, and nothing sweeps it", n)
	}
}

// TestRawShapeMismatchIsRefused is what stops the content-type branch inside the
// worker arm from becoming the role-confusion bug.
//
// The job's STAMPED mode is the authority. A header can select a representation
// after the principal's role and the job's mode are both settled; it can never
// choose the mode.
func TestRawShapeMismatchIsRefused(t *testing.T) {
	t.Run("octet-stream for a units job", func(t *testing.T) {
		e := newEnv(t)
		e.liveWorker(t, "ocr")
		e.unitsService(t, "ocr", 3)

		body, ct := multipartBody(t, "in.pdf", "source")
		resp := e.do(t, "POST", "/upload?label=ocr", e.clientTok, body, ct)
		var created struct {
			JobID string `json:"job_id"`
		}
		decodeJSON(t, resp, &created)
		e.do(t, "POST", "/claim?label=ocr&raw=0", e.workerTok, nil, "")

		post := e.do(t, "POST", "/upload?job_id="+created.JobID, e.workerTok,
			bytes.NewReader([]byte(pngMagic)), "application/octet-stream")
		if post.StatusCode == http.StatusNoContent {
			t.Error("a worker posted raw bytes for a UNITS job and was accepted — the header " +
				"chose the shape, which the stamped mode must decide")
		}
	})

	t.Run("units JSON for a raw job", func(t *testing.T) {
		e := newEnv(t)
		id := e.rawJobInFlight(t, "convert")

		post := e.do(t, "POST", "/upload", e.workerTok,
			bytes.NewReader([]byte(`{"job_id":"`+id+`","units":["a"]}`)), "application/json")
		if post.StatusCode == http.StatusNoContent {
			t.Error("a worker posted units for a RAW job and was accepted")
		}
	})
}

// TestLeaseStillGuardsRawCompletion: the second write path must carry the same
// authorisation as the first. A lease check added to one and forgotten on the
// other is how one customer's worker writes another's result.
func TestLeaseStillGuardsRawCompletion(t *testing.T) {
	e := newEnv(t)
	id := e.rawJobInFlight(t, "convert")

	// A DIFFERENT worker token, which holds no lease on this job.
	other := e.mintWorker(t, "w2@example.com")
	post := e.do(t, "POST", "/upload?job_id="+id, other,
		bytes.NewReader([]byte(pngMagic)), "application/octet-stream")
	if post.StatusCode == http.StatusNoContent {
		t.Error("a worker without the lease completed the job — the raw path must carry the " +
			"same authorisation as the units path")
	}

	// And the rightful holder still can.
	if ok := e.do(t, "POST", "/upload?job_id="+id, e.workerTok,
		bytes.NewReader([]byte(pngMagic)), "application/octet-stream"); ok.StatusCode != http.StatusNoContent {
		t.Errorf("the lease holder was refused (%d) — the guard must not refuse everything", ok.StatusCode)
	}
}

// TestRawResultNeverEntersResultsStore pins the memory property: the largest
// outputs are the ones that cost the router no RAM.
func TestRawResultNeverEntersResultsStore(t *testing.T) {
	e := newEnv(t)
	id := e.rawJobInFlight(t, "convert")

	e.do(t, "POST", "/upload?job_id="+id, e.workerTok,
		bytes.NewReader([]byte(pngMagic)), "application/octet-stream")

	if _, ok := e.results.Peek(id); ok {
		t.Error("a raw result entered the in-memory results store — ADR-0001 already lists that " +
			"store as the system's unbounded-growth risk, and a binary payload is the worst case")
	}
	if _, err := e.repo.JobByID(context.Background(), id); err != nil {
		t.Fatalf("JobByID: %v", err)
	}
}
