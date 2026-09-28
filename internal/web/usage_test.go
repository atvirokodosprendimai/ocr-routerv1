package web_test

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestUsageSectionRendersEveryCustomer is the first paint.
//
// It is red if `Dashboard.Usage` is filled in the handler rather than in
// buildDashboard — which renders once and then blanks itself on the first SSE
// event, because the patch path reads the same read model.
func TestUsageSectionRendersEveryCustomer(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)

	page := e.page(t, "/admin/users")
	if !strings.Contains(page, `id="usage"`) {
		t.Fatal("no #usage section on the Customers page — a fragment cannot be patched into " +
			"existence, so its id has to be in the first paint")
	}
	if !strings.Contains(page, "c@example.com") {
		t.Error("the usage section does not name the customer it is about")
	}
}

// TestUsageNumbersAreLabelledNotBare is the accessibility half, and it is the one
// a "does it render" test cannot see.
//
// Sixteen numbers per customer is only legible if each one says which window and
// which bucket it belongs to. A digit whose meaning lives in a header two rows up
// is not readable on a narrow screen and is not readable at all by a screen reader
// that has left the header behind.
//
// ⚠ IT READS THE #usage SECTION, NOT THE PAGE, and that is a correction: the first
// version grepped the whole document for "delivered", "failed" and "expired" — all
// three of which are JOB STATES and appear in the Stats cards on any page that
// renders them. The test passed on words this section never produced. A mutation
// hiding the breakdown survived it, which is how the weakness surfaced.
func TestUsageNumbersAreLabelledNotBare(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)

	section := usageSection(t, e.page(t, "/admin/users"))

	// seedJobs creates three jobs at `base` and marks two of them dead, and the
	// env's clock is pinned to `base` — so the client's 24h window is 3 pushed,
	// 0 delivered, 2 failed, 0 expired.
	//
	// ⚠ ZERO DELIVERED IS THE FIXTURE, NOT A BUG, and it is worth stating: this
	// test first asserted 1 delivered because seedJobs constructs a job with
	// `State: core.JobDelivered`. `Repo.CreateJob` IGNORES that field and writes
	// `queued` unconditionally, so no delivered job can be seeded through it. The
	// delivered bucket's semantics are T1's subject and are covered there against
	// raw-seeded rows; what this test owns is the RENDERING.
	//
	// Asserting the rendered SENTENCE rather than the words is the other half:
	// "delivered", "failed" and "expired" are job states and appear in the Stats
	// cards too, so a page-wide grep for them passes on text this section never
	// produced.
	if !strings.Contains(section, "3 pushed") {
		t.Errorf("the usage section does not render `3 pushed` for the seeded customer: %q",
			clip(section))
	}
	if !strings.Contains(section, "0 delivered · 2 failed · 0 expired") {
		t.Errorf("the usage section does not render the breakdown as a labelled sentence: %q",
			clip(section))
	}
	for _, want := range []string{"24h", "7d", "31d"} {
		if !strings.Contains(section, want) {
			t.Errorf("the usage section never says %q, so its columns are unlabelled", want)
		}
	}
	// The aria-label is what a screen reader hears once the header has scrolled
	// out of the viewport, so it has to name the customer and the window too.
	if !strings.Contains(section, "over the last 24 hours") {
		t.Errorf("no cell names its window in words for a screen reader: %q", clip(section))
	}
}

// usageSection returns just the #usage fragment, so an assertion cannot be
// satisfied by text from elsewhere on the page.
func usageSection(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="usage"`)
	if i < 0 {
		t.Fatal("no #usage section on the page, so this assertion proved nothing")
	}
	end := strings.Index(page[i:], "</section>")
	if end < 0 {
		t.Fatal("the #usage section is never closed")
	}
	return page[i : i+end]
}

// TestTheCustomersPageSubscribesToTheStream is the half that makes "live" true.
//
// The Customers page has never carried a data-init, so counters patched by the
// stream would be live in a stream nobody joined — working code, and an
// unchanging screen.
func TestTheCustomersPageSubscribesToTheStream(t *testing.T) {
	e := newEnv(t)

	page := e.page(t, "/admin/users")
	if !strings.Contains(page, "data-init") {
		t.Fatal("the Customers page does not subscribe to the stream, so nothing it renders " +
			"can update")
	}
	if !strings.Contains(page, "/admin/stream") {
		t.Error("the subscription does not name the stream route")
	}
}

// TestTheStreamPatchesUsage observes liveness rather than asserting it.
func TestTheStreamPatchesUsage(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)

	body := readStream(t, e)
	if !strings.Contains(body, "usage") {
		t.Errorf("the stream sent no usage fragment in its first push: %q", clip(body))
	}
}

// TestTheStreamDoesNotPatchTheUserTable is the NEGATIVE that protects an operator
// mid-edit, and it is the most valuable test in this file.
//
// The obvious implementation patches the table the counters are about. That table
// carries every row's buffer-limit, priority, TTL, credit-delta and reason inputs,
// so re-rendering it every fifteen seconds lands under someone who is typing in
// one — and a known defect already has bound inputs refilling from stale signals
// after a morph. ADR-0010 decision 3 is this test.
func TestTheStreamDoesNotPatchTheUserTable(t *testing.T) {
	e := newEnv(t)
	seedUsage(t, e)

	body := readStream(t, e)
	if strings.Contains(body, "user-table") {
		t.Errorf("the stream patched #user-table, which re-renders every row's edit inputs "+
			"under an operator who may be typing in one: %q", clip(body))
	}
}

// seedUsage gives the client customer one job, so the counters have something to
// count and a zero-everywhere table cannot pass these tests vacuously.
//
// It reuses `seedJobs`, which already puts one delivered job and two dead ones in
// the store at `base` — three of the four buckets, from a helper that exists.
func seedUsage(t *testing.T, e *env) {
	t.Helper()
	e.seedJobs(t)
}

// readStream opens the admin stream, reads its first push, and closes it.
func readStream(t *testing.T, e *env) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/admin/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/stream = %d, want 200", resp.StatusCode)
	}

	// The first push is written immediately; read what is buffered and stop
	// rather than waiting for the connection to end, because it never does.
	var b strings.Builder
	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			break
		}
		// Three fragments are pushed per cycle; once the stream has gone quiet
		// after them, stop. A blank line ends an SSE event.
		if strings.Count(b.String(), "event:") >= 4 {
			break
		}
	}
	return b.String()
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
