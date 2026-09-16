package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAdminCanMarkServiceRaw is the administrator third of ADR-0006's mode
// agreement.
//
// Without this control `service_rates.raw` is settable only by SQL, and the
// agreement has an administrator who cannot administer: every raw worker is
// refused and the feature looks broken rather than unconfigured.
func TestAdminCanMarkServiceRaw(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	resp := e.do(t, "POST", "/admin/rates", e.adminTok,
		strings.NewReader(`{"rateLabel":"convert","rateValue":"1","rateRaw":true}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setting a raw rate = %d, want 200", resp.StatusCode)
	}

	rate, raw, err := e.repo.ServiceMode(ctx, "convert")
	if err != nil {
		t.Fatalf("ServiceMode: %v", err)
	}
	if !raw || rate != 1 {
		t.Errorf("ServiceMode = (%d, %v), want (1, true) — the control did not reach SetRate", rate, raw)
	}

	// And it must be clearable, or a service can be promoted to flat-rate
	// billing and never demoted.
	resp = e.do(t, "POST", "/admin/rates", e.adminTok,
		strings.NewReader(`{"rateLabel":"convert","rateValue":"1","rateRaw":false}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clearing raw = %d, want 200", resp.StatusCode)
	}
	if _, raw, _ = e.repo.ServiceMode(ctx, "convert"); raw {
		t.Error("raw could not be cleared once set")
	}
}

// TestRateStillSettableWithoutTouchingMode guards the placeholder T1 installed.
//
// SetRate upserts the WHOLE row, so a rate edit that did not carry the current
// mode would silently demote every raw service the next time anyone adjusted its
// price — a repricing nobody asked for and nothing would report.
func TestRateStillSettableWithoutTouchingMode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := e.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}

	// An admin changing only the price, with the raw box checked as the page
	// would render it for an already-raw service.
	resp := e.do(t, "POST", "/admin/rates", e.adminTok,
		strings.NewReader(`{"rateLabel":"convert","rateValue":"7","rateRaw":true}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rate edit = %d, want 200", resp.StatusCode)
	}

	rate, raw, err := e.repo.ServiceMode(ctx, "convert")
	if err != nil {
		t.Fatalf("ServiceMode: %v", err)
	}
	if rate != 7 {
		t.Errorf("rate = %d, want 7", rate)
	}
	if !raw {
		t.Error("a price edit silently demoted a raw service to units")
	}
}

// TestServicesPageShowsMode is the discoverability rung.
//
// An admin-owned flag nobody can SEE is one nobody will set, and every worker on
// that label is then refused with no visible cause. The table already carries a
// Rate column for the same reason.
func TestServicesPageShowsMode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.repo.SetRate(ctx, "convert", 1, true, base); err != nil {
		t.Fatalf("SetRate convert: %v", err)
	}
	if err := e.repo.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate ocr: %v", err)
	}

	resp := e.do(t, "GET", "/admin/services", e.adminTok, nil)
	if resp.StatusCode != http.StatusOK {
	}
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	// ⚠ Assert on the TABLE's badge, not on the word "raw".
	//
	// A mutation caught this: `strings.Contains(page, "raw")` passed with the
	// Mode column deleted entirely, because the rate form's own label ("Raw
	// output"), its id and its `data-bind:rate-raw` attribute all carry the word.
	// The form could carry the verdict by itself, so the assertion measured the
	// wrong subject. `class="tag raw"` exists only in the services table.
	if strings.Count(page, `class="tag raw"`) != 1 {
		t.Errorf("the services table shows the raw badge %d time(s), want exactly 1 — the Mode "+
			"column is what tells an admin which services are raw",
			strings.Count(page, `class="tag raw"`))
	}
	if !strings.Contains(page, ">units<") {
		t.Error("the services table does not mark the units service — a Mode column that renders " +
			"only one of the two modes says nothing about the other")
	}
	// Both services must be listed, or the assertions above could hold on a page
	// that simply omitted one of them.
	for _, label := range []string{"convert", "ocr"} {
		if !strings.Contains(page, label) {
			t.Errorf("service %q is not on the page at all", label)
		}
	}
}
