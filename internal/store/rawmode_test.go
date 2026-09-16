package store_test

import (
	"context"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/store"
)

// TestSetRateCarriesRawMode pins the write half of the admin-owned mode.
//
// ADR-0006 puts `raw` in service_rates rather than letting a worker declare it,
// because raw is a PRICE — flat 1 credit instead of len(units) × rate — and
// ADR-0001 already recorded worker-declared pricing as a privilege escalation.
// This is the test that the bit is admin-writable at all.
func TestSetRateCarriesRawMode(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	if err := r.SetRate(ctx, "convert", 5, true, base); err != nil {
		t.Fatalf("SetRate raw: %v", err)
	}
	rate, raw, err := r.ServiceMode(ctx, "convert")
	if err != nil {
		t.Fatalf("ServiceMode: %v", err)
	}
	if rate != 5 || !raw {
		t.Errorf("ServiceMode = (%d, %v), want (5, true)", rate, raw)
	}

	// Clearing it must work too: a service demoted out of raw mode that stayed
	// raw would keep billing a flat credit for unit-split output.
	if err := r.SetRate(ctx, "convert", 5, false, base); err != nil {
		t.Fatalf("SetRate units: %v", err)
	}
	if _, raw, err = r.ServiceMode(ctx, "convert"); err != nil || raw {
		t.Errorf("ServiceMode raw after clearing = %v (err %v), want false", raw, err)
	}
}

// TestServiceModeDefaultsToUnits is the invariant the whole ADR is shaped
// around: an existing paid service must never be silently promoted to flat-1
// pricing by the migration that adds the column.
func TestServiceModeDefaultsToUnits(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()

	// A label nobody has configured.
	rate, raw, err := r.ServiceMode(ctx, "never-configured")
	if err != nil {
		t.Fatalf("ServiceMode: %v", err)
	}
	if rate != 1 || raw {
		t.Errorf("unconfigured label = (%d, %v), want (1, false)", rate, raw)
	}

	// A row that exists with a rate — the shape every pre-migration deployment
	// has. The column's DEFAULT is what decides this, so it is the migration
	// under test as much as the reader.
	if err := r.SetRate(ctx, "ocr", 3, false, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	if rate, raw, err = r.ServiceMode(ctx, "ocr"); err != nil || rate != 3 || raw {
		t.Errorf("configured units label = (%d, %v) err %v, want (3, false)", rate, raw, err)
	}
}

// TestCreateJobPersistsRawStamp covers the immutable per-job half.
//
// The job's mode is stamped at admission so that an administrator editing
// service_rates mid-flight cannot reprice a job that is already running.
func TestCreateJobPersistsRawStamp(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	mkUser(t, r, "u", 0, 10)

	j := core.Job{
		ID: "job-raw", UserID: "u", Filename: "f.bin", Label: "convert",
		Pipeline: []string{"convert"}, Stage: 0, Params: map[string]string{},
		HasBlob: true, Raw: true, State: core.JobQueued,
		QueuedAt: base, CreatedAt: base, UpdatedAt: base,
	}
	if err := r.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	got, err := r.JobByID(ctx, "job-raw")
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if !got.Raw {
		t.Errorf("job.Raw = false after being created raw — the stamp did not persist")
	}

	// And the default is units, for exactly the same reason as the rate default.
	plain := mkJob(t, r, "job-units", "u", "ocr", base, base)
	if got, err = r.JobByID(ctx, plain.ID); err != nil || got.Raw {
		t.Errorf("job.Raw = %v for a job created without one, want false (err %v)", got.Raw, err)
	}
}

// TestRateForLabelStillAnswers guards the reuse: ServiceMode subsumes
// RateForLabel, and the older reader has live callers that must keep working.
func TestRateForLabelStillAnswers(t *testing.T) {
	r := newRepo(t)
	ctx := context.Background()
	if err := r.SetRate(ctx, "ocr", 7, false, base); err != nil {
		t.Fatalf("SetRate: %v", err)
	}
	rate, err := r.RateForLabel(ctx, "ocr")
	if err != nil || rate != 7 {
		t.Errorf("RateForLabel = %d (err %v), want 7", rate, err)
	}
	if rate, err = r.RateForLabel(ctx, "unconfigured"); err != nil || rate != 1 {
		t.Errorf("RateForLabel(unconfigured) = %d (err %v), want 1", rate, err)
	}
}

var _ = store.Repo{}
