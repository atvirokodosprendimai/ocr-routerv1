package agent

import "testing"

// TestInflightKeepsTwoClaimsOfOneJobApart is ADR-0013 on the worker: two slots
// holding one job under two claims (a reclaim between them) each finish or
// release their own claim, and the stale one finishing leaves the current one
// to be handed back on shutdown.
func TestInflightKeepsTwoClaimsOfOneJobApart(t *testing.T) {
	i := newInflight()
	i.add("j", "lease-old")
	i.add("j", "lease-new")
	i.done("j", "lease-old")

	got := i.snapshot()
	if len(got) != 1 || got[0] != (claim{"j", "lease-new"}) {
		t.Errorf("after the stale claim finished, held = %v, want only {j lease-new}", got)
	}
}
