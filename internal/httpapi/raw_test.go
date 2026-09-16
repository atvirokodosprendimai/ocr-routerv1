package httpapi_test

import (
	"context"
	"net/http"
	"testing"
)

// rawService marks a label raw in the admin-owned service_rates row.
//
// It goes through SetRate rather than raw SQL on purpose: that is the ONLY
// writer of a service's mode, and a test that reached around it would be
// asserting against a state the system cannot actually produce.
func (e *env) rawService(t *testing.T, label string, rate int) {
	t.Helper()
	if err := e.repo.SetRate(context.Background(), label, rate, true, base); err != nil {
		t.Fatalf("SetRate(%s, raw): %v", label, err)
	}
}

func (e *env) unitsService(t *testing.T, label string, rate int) {
	t.Helper()
	if err := e.repo.SetRate(context.Background(), label, rate, false, base); err != nil {
		t.Fatalf("SetRate(%s, units): %v", label, err)
	}
}

// TestRawModeMismatchIsRefused is ADR-0006's security boundary, from the worker
// side.
//
// `raw` is a PRICE — a flat credit instead of len(units) × rate — so a worker
// able to declare it would be a worker able to set what customers are charged.
// ADR-0001 drew that line for credits_per_unit; this keeps the mode on the same
// side of it. BOTH directions are checked: a one-way check would leave a leaked
// worker token free to downgrade a paid label to flat-rate billing, which is the
// direction that actually costs money.
func TestRawModeMismatchIsRefused(t *testing.T) {
	e := newEnv(t)
	e.rawService(t, "convert", 1)
	e.unitsService(t, "ocr", 3)

	cases := []struct {
		name  string
		query string
	}{
		{"a raw worker on a units label", "/claim?label=ocr&raw=1"},
		{"a units worker on a raw label", "/claim?label=convert&raw=0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := e.do(t, "POST", c.query, e.workerTok, nil, "")
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("%s = %d, want 409 — the worker's declaration disagrees with the "+
					"admin record and must be refused, not recorded", c.name, resp.StatusCode)
			}
		})
	}

	// The same refusal on the SSE declaration path. A worker that is refused on
	// /claim but accepted on /sse would still register its label.
	resp := e.do(t, "GET", "/sse?label=ocr&raw=1", e.workerTok, nil, "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("SSE subscribe with a mismatched mode = %d, want 409", resp.StatusCode)
	}
}

// TestMatchingModeIsAccepted is the other half: agreement must still work, or
// the refusal above is satisfied by refusing everything.
func TestMatchingModeIsAccepted(t *testing.T) {
	e := newEnv(t)
	e.rawService(t, "convert", 1)
	e.unitsService(t, "ocr", 3)

	for _, q := range []string{"/claim?label=convert&raw=1", "/claim?label=ocr&raw=0"} {
		resp := e.do(t, "POST", q, e.workerTok, nil, "")
		// 204 is the normal answer to a polling worker on an empty queue.
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s = %d, want 204 — an agreeing worker must be admitted", q, resp.StatusCode)
		}
	}
}

// TestUndeclaredModeIsUnits pins what silence means.
//
// An un-upgraded worker sends no `raw` at all. It must read as UNITS, never as
// "whatever the label says" — inferring the mode from the record would make an
// old worker silently correct on a raw label, which is exactly the escalation
// the three-party agreement exists to prevent.
func TestUndeclaredModeIsUnits(t *testing.T) {
	e := newEnv(t)
	e.rawService(t, "convert", 1)
	e.unitsService(t, "ocr", 3)

	if resp := e.do(t, "POST", "/claim?label=ocr", e.workerTok, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("an undeclared worker on a units label = %d, want 204", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/claim?label=convert", e.workerTok, nil, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("an undeclared worker on a RAW label = %d, want 409 — silence means units, "+
			"so an un-upgraded worker must be refused rather than quietly accepted", resp.StatusCode)
	}
}

// TestUnconfiguredLabelIsUnits covers the shape every new service has: no
// service_rates row at all. It must behave as units for the same reason the rate
// defaults to 1 rather than 0.
func TestUnconfiguredLabelIsUnits(t *testing.T) {
	e := newEnv(t)

	if resp := e.do(t, "POST", "/claim?label=brand-new", e.workerTok, nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("an undeclared worker on an unconfigured label = %d, want 204", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/claim?label=brand-new&raw=1", e.workerTok, nil, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("a RAW worker on an unconfigured label = %d, want 409 — raw is admin-owned, so "+
			"a service nobody configured is not raw", resp.StatusCode)
	}
}
