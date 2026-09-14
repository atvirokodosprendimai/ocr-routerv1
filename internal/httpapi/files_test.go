package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/router"
)

// seedJob uploads a job through the service and returns it.
func seedJob(t *testing.T, e *env, label string, body string) core.Job {
	t.Helper()
	in := router.UploadInput{Filename: "f.pdf", Pipeline: []string{label}}
	if body != "" {
		in.Body = strings.NewReader(body)
	}
	j, err := e.rt.Upload(context.Background(), e.clientID, in, base)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	return j
}

func TestFilesClientGetsResultAndIsCharged(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()
	j := seedJob(t, e, "ocr", "source")

	// Run it through a worker so a result exists.
	claimed, err := e.rt.Claim(ctx, "w-token", "ocr", base)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := e.rt.Complete(ctx, "w-token", claimed.ID, []string{"p1", "p2"}, base); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	before, _ := e.repo.UserByID(ctx, e.clientID)
	resp := e.do(t, "GET", "/files/"+j.ID, e.clientTok, nil, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /files = %d (%s), want 200", resp.StatusCode, b)
	}
	var got struct {
		JobID string   `json:"job_id"`
		Units []string `json:"units"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got.Units) != 2 || got.Units[0] != "p1" {
		t.Errorf("units = %v", got.Units)
	}

	after, _ := e.repo.UserByID(ctx, e.clientID)
	if after.Credits != before.Credits-2 {
		t.Errorf("credits went %d -> %d, want a debit of 2", before.Credits, after.Credits)
	}
}

func TestFilesClientSecondFetchIs404(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()
	j := seedJob(t, e, "ocr", "source")

	claimed, _ := e.rt.Claim(ctx, "w-token", "ocr", base)
	_ = e.rt.Complete(ctx, "w-token", claimed.ID, []string{"p1"}, base)

	if resp := e.do(t, "GET", "/files/"+j.ID, e.clientTok, nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("first fetch = %d, want 200", resp.StatusCode)
	}
	resp := e.do(t, "GET", "/files/"+j.ID, e.clientTok, nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second fetch = %d, want 404 — delivery is idempotent to the holder, and the "+
			"second call must not charge again", resp.StatusCode)
	}

	// One charge, not two.
	entries, _ := e.repo.Ledger(ctx, e.clientID, 10)
	charges := 0
	for _, en := range entries {
		if en.Delta < 0 {
			charges++
		}
	}
	if charges != 1 {
		t.Errorf("%d debit rows after two fetches, want 1", charges)
	}
}

func TestFilesRejectsOtherUsersJob(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()

	// A second customer with their own token.
	ap, _ := e.ident.Authenticate(ctx, "Bearer "+e.adminTok, base)
	other, _ := e.ident.CreateUser(ctx, ap, "other@example.com", core.RoleClient, base)
	_, otherTok, _ := e.ident.MintToken(ctx, ap, other.ID, core.RoleClient, "o", base)

	j := seedJob(t, e, "ocr", "source")
	claimed, _ := e.rt.Claim(ctx, "w-token", "ocr", base)
	_ = e.rt.Complete(ctx, "w-token", claimed.ID, []string{"p1"}, base)

	resp := e.do(t, "GET", "/files/"+j.ID, otherTok, nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("another customer fetching = %d, want 404 (not 403 — they must not learn the "+
			"id exists)", resp.StatusCode)
	}
}

func TestFilesWorkerNeedsLease(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()
	j := seedJob(t, e, "ocr", "the source bytes")

	// Not yet claimed: no lease, no bytes.
	if resp := e.do(t, "GET", "/files/"+j.ID, e.workerTok, nil, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("worker fetching an unclaimed job = %d, want 409", resp.StatusCode)
	}

	// Claim it AS THIS WORKER TOKEN, which is what the handler checks.
	p, _ := e.ident.Authenticate(ctx, "Bearer "+e.workerTok, base)
	if _, err := e.rt.Claim(ctx, p.TokenID, "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	resp := e.do(t, "GET", "/files/"+j.ID, e.workerTok, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease holder fetching = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "the source bytes" {
		t.Errorf("blob body = %q", b)
	}
}

func TestFilesRejectsNonUUIDPath(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"..", "not-an-id", "0192f8aa-XXXX-7000-8000-000000000000"} {
		resp := e.do(t, "GET", "/files/"+id, e.clientTok, nil, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /files/%s = %d, want 404", id, resp.StatusCode)
		}
	}
}

func TestClaimEmptyQueueIs204(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	resp := e.do(t, "POST", "/claim?label=ocr", e.workerTok, nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("claiming an empty queue = %d, want 204 — an empty queue is the NORMAL answer "+
			"to a polling worker, not an error to be logged", resp.StatusCode)
	}
}

func TestClaimIsLabelScopedAndCarriesParams(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	e.liveWorker(t, "crawl")

	seedJob(t, e, "ocr", "src")
	// A crawler job with params and no blob.
	if _, err := e.rt.Upload(context.Background(), e.clientID, router.UploadInput{
		Pipeline: []string{"crawl"}, Params: map[string]string{"url": "https://example.com"},
	}, base); err != nil {
		t.Fatalf("Upload crawl: %v", err)
	}

	resp := e.do(t, "POST", "/claim?label=crawl", e.workerTok, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Label   string            `json:"label"`
		Params  map[string]string `json:"params"`
		HasBlob bool              `json:"has_blob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Label != "crawl" {
		t.Errorf("a crawl worker was handed a %q job", got.Label)
	}
	if got.Params["url"] != "https://example.com" {
		t.Errorf("params = %v — the claim must carry everything the worker needs to build its "+
			"argv, without a second request", got.Params)
	}
	if got.HasBlob {
		t.Error("has_blob = true for a params-only job")
	}
}

func TestUploadWorkerCompletesJob(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()
	j := seedJob(t, e, "ocr", "src")

	p, _ := e.ident.Authenticate(ctx, "Bearer "+e.workerTok, base)
	if _, err := e.rt.Claim(ctx, p.TokenID, "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	body := strings.NewReader(`{"job_id":"` + j.ID + `","units":["a","b"]}`)
	resp := e.do(t, "POST", "/upload", e.workerTok, body, "application/json")
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("worker result = %d (%s), want 204", resp.StatusCode, b)
	}
	got, _ := e.repo.JobByID(ctx, j.ID)
	if got.State != core.JobDone || got.Units != 2 {
		t.Errorf("job = state %q units %d, want done/2", got.State, got.Units)
	}
}

func TestUploadWorkerReportsFailure(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	ctx := context.Background()
	j := seedJob(t, e, "ocr", "src")

	p, _ := e.ident.Authenticate(ctx, "Bearer "+e.workerTok, base)
	if _, err := e.rt.Claim(ctx, p.TokenID, "ocr", base); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	body := strings.NewReader(`{"job_id":"` + j.ID + `","error":"exit status 1"}`)
	resp := e.do(t, "POST", "/upload", e.workerTok, body, "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("worker failure = %d, want 204", resp.StatusCode)
	}
	got, _ := e.repo.JobByID(ctx, j.ID)
	if got.State != core.JobQueued {
		t.Errorf("state after a reported failure = %q, want queued (attempts remain)", got.State)
	}
	if got.LastError == "" {
		t.Error("the worker's error was not recorded — the dashboard would show no reason")
	}
}

func TestServicesListsLiveLabels(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")
	e.liveWorker(t, "crawl")

	resp := e.do(t, "GET", "/services", e.clientTok, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /services = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "crawl" || got.Labels[1] != "ocr" {
		t.Errorf("labels = %v, want [crawl ocr] sorted — a client reading this must not then "+
			"be surprised by an unknown-label refusal", got.Labels)
	}
}
