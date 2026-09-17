package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// unitsJobInFlight admits a units job and leases it to the worker.
func (e *env) unitsJobInFlight(t *testing.T, label string) string {
	t.Helper()
	e.liveWorker(t, label)
	e.unitsService(t, label, 3)

	body, ct := multipartBody(t, "in.pdf", "source")
	resp := e.do(t, "POST", "/upload?label="+label, e.clientTok, body, ct)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	decodeJSON(t, resp, &created)

	if c := e.do(t, "POST", "/claim?label="+label+"&raw=0", e.workerTok, nil, ""); c.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", c.StatusCode)
	}
	return created.JobID
}

// TestExitCodeSurvivesToTheJobRow is ADR-0007's whole point, asserted across
// every hop rather than at one.
func TestExitCodeSurvivesToTheJobRow(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	resp := e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader([]byte(
		`{"job_id":"`+id+`","error":"exit status 3: stderr: boom","exit_code":3}`)),
		"application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reporting a failure = %d, want 204", resp.StatusCode)
	}

	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.ExitCode == nil || *job.ExitCode != 3 {
		t.Errorf("ExitCode on the job row = %v, want 3", job.ExitCode)
	}
	if job.LastError == "" {
		t.Error("last_error is empty — this task adds a field BESIDE the message, it does not " +
			"replace it")
	}
}

// TestTimeoutLeavesNoExitCode: absent on the wire must stay absent in the column.
//
// Nothing in the chain may turn a missing code into 0, or every codeless failure
// reads as a clean exit at the far end.
func TestTimeoutLeavesNoExitCode(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	resp := e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader([]byte(
		`{"job_id":"`+id+`","error":"timed out after 5m"}`)), "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reporting a failure = %d, want 204", resp.StatusCode)
	}

	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.ExitCode != nil {
		t.Errorf("a failure reported with NO exit code stored %d — absent on the wire must stay "+
			"absent in the column", *job.ExitCode)
	}
}

// TestUnitsFailureUnchanged: the message an operator already reads keeps working.
func TestUnitsFailureUnchanged(t *testing.T) {
	e := newEnv(t)
	id := e.unitsJobInFlight(t, "ocr")

	e.do(t, "POST", "/upload", e.workerTok, bytes.NewReader([]byte(
		`{"job_id":"`+id+`","error":"stdout is not a JSON array of strings"}`)), "application/json")

	job, err := e.repo.JobByID(context.Background(), id)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if job.LastError != "stdout is not a JSON array of strings" {
		t.Errorf("last_error = %q — this task must not change what it carries", job.LastError)
	}
}
