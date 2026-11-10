package router

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/bus"
	"github.com/atvirokodosprendimai/ocr-routerv1/internal/core"
)

// TestLostLeaseIsAConflict: a guarded writer that matched no row reaches the
// worker as 409, the answer holdsLease gives for the same fact (F4/F5 review);
// any other error passes through untouched.
func TestLostLeaseIsAConflict(t *testing.T) {
	if got := lostLease(core.ErrNotFound); !errors.Is(got, core.ErrConflict) {
		t.Errorf("lostLease(ErrNotFound) = %v, want core.ErrConflict", got)
	}
	other := errors.New("disk full")
	if got := lostLease(other); got != other {
		t.Errorf("lostLease(other) = %v, want it unchanged", got)
	}
}

// TestAvailableLabelsPrunesExpiredLabels is F14 (REPORT 2026-11-10): Claim
// stamps any label a worker names, and nothing deleted from labelSeen, so a
// worker token claiming random labels grew the map for ever and every upload
// scanned it. Entries past the grace window are dropped when the map is read.
func TestAvailableLabelsPrunesExpiredLabels(t *testing.T) {
	s := New(nil, nil, nil, bus.New(), Config{LabelGrace: 5 * time.Minute})
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		s.noteLabel(fmt.Sprintf("random-%d", i), at)
	}
	if got := s.AvailableLabels(at.Add(10 * time.Minute)); len(got) != 0 {
		t.Errorf("AvailableLabels past the grace window = %d labels, want 0", len(got))
	}
	s.mu.Lock()
	n := len(s.labelSeen)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("labelSeen holds %d labels past their grace window, want 0", n)
	}
}
