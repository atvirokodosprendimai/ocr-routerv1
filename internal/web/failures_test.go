package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

func intp(n int) *int { return &n }

// seedJobs puts one delivered job and two failures in the store: one that exited
// 3, and one that never exited at all.
func (e *env) seedJobs(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	for _, j := range []core.Job{
		{ID: "job-ok", UserID: e.clientID, Filename: "a.pdf", Label: "ocr",
			Pipeline: []string{"ocr"}, Params: map[string]string{}, State: core.JobDelivered,
			Units: 4, AccruedCredits: 12, QueuedAt: base, CreatedAt: base, UpdatedAt: base},
		{ID: "job-exit3", UserID: e.clientID, Filename: "b.pdf", Label: "ocr",
			Pipeline: []string{"ocr"}, Params: map[string]string{}, State: core.JobQueued,
			QueuedAt: base, CreatedAt: base, UpdatedAt: base},
		{ID: "job-timeout", UserID: e.clientID, Filename: "c.pdf", Label: "ocr",
			Pipeline: []string{"ocr"}, Params: map[string]string{}, State: core.JobQueued,
			QueuedAt: base, CreatedAt: base, UpdatedAt: base},
	} {
		if err := e.repo.CreateJob(ctx, j); err != nil {
			t.Fatalf("CreateJob(%s): %v", j.ID, err)
		}
	}
	if err := e.repo.FailJobDead(ctx, "job-exit3", "exit status 3: stderr: boom", intp(3), base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}
	if err := e.repo.FailJobDead(ctx, "job-timeout", "timed out after 5m", nil, base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}
}

func (e *env) page(t *testing.T, path string) string {
	t.Helper()
	resp := e.do(t, "GET", path, e.adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestFailedFilterShowsOnlyFailures asserts BOTH halves.
//
// "shows failures" alone is satisfied by a page that shows everything, which is
// exactly the page this view exists to improve on.
func TestFailedFilterShowsOnlyFailures(t *testing.T) {
	e := newEnv(t)
	e.seedJobs(t)

	page := e.page(t, "/admin/?state=failed")
	if !strings.Contains(page, "job-exi") {
		t.Error("the failures view does not list a dead job")
	}
	if strings.Contains(page, "job-ok") {
		t.Error("the failures view lists a DELIVERED job — then it is the dashboard with a " +
			"different name, and answers the same question badly")
	}
}

// TestExitCodeIsShownDistinctlyFromNoExit carries T1's nullable column all the
// way to the one place a human reads it.
func TestExitCodeIsShownDistinctlyFromNoExit(t *testing.T) {
	e := newEnv(t)
	e.seedJobs(t)

	page := e.page(t, "/admin/?state=failed")
	if !strings.Contains(page, `class="exit">3<`) {
		t.Error("a job that exited 3 does not show its code")
	}
	// Scoped to the exit cell on purpose: a bare `>0<` matches any zero on the
	// page (a units column, a credit count) and would pass on a page with no
	// exit column at all.
	if strings.Contains(page, `class="exit">0<`) {
		t.Error("a failure with no exit code rendered as 0 — a job that never exited must not " +
			"look like one that exited cleanly")
	}
	if !strings.Contains(page, `class="exit">—<`) {
		t.Error("a failure with no exit code shows nothing recognisable — the cell must say " +
			"positively that there was no exit")
	}
}

// TestFailuresViewUsesTheSameReadModel: a second query would drift, and the
// first divergence would be invisible.
func TestFailuresViewUsesTheSameReadModel(t *testing.T) {
	e := newEnv(t)
	e.seedJobs(t)

	all := e.page(t, "/admin/")
	failed := e.page(t, "/admin/?state=failed")

	// The dead job's cause appears identically on both — same rows, filtered.
	const cause = "exit status 3"
	if !strings.Contains(all, cause) || !strings.Contains(failed, cause) {
		t.Errorf("the cause is on the dashboard: %v, on the filter: %v — the filter must be over "+
			"the same read model, not a second query",
			strings.Contains(all, cause), strings.Contains(failed, cause))
	}
}

// TestDetailDoesNotBreakTheTable asserts on the markup that bounds the cell,
// not on the eye.
func TestDetailDoesNotBreakTheTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.repo.CreateJob(ctx, core.Job{
		ID: "job-long", UserID: e.clientID, Filename: "d.pdf", Label: "ocr",
		Pipeline: []string{"ocr"}, Params: map[string]string{}, State: core.JobQueued,
		QueuedAt: base, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := e.repo.FailJobDead(ctx, "job-long", strings.Repeat("x", 2000), intp(1), base); err != nil {
		t.Fatalf("FailJobDead: %v", err)
	}

	page := e.page(t, "/admin/?state=failed")
	if !strings.Contains(page, `class="detail"`) {
		t.Error("the Detail cell carries no bounding class — a 2000-byte stream then stretches " +
			"the table it lands in, which is the defect that exists today")
	}
}
