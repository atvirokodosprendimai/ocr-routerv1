package results_test

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/results"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// clock is a movable time source, so expiry is asserted deterministically
// instead of by sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(ttl time.Duration) (*results.Store, *clock) {
	c := &clock{t: base}
	s := results.New(ttl)
	s.SetClock(c.now)
	return s, c
}

func TestResultTakeReturnsOnce(t *testing.T) {
	s, _ := newStore(time.Hour)
	s.Put("j1", []string{"page one", "page two"})

	got, err := s.Take("j1")
	if err != nil {
		t.Fatalf("first Take: %v", err)
	}
	if got.JobID != "j1" || len(got.Units) != 2 || got.Units[0] != "page one" {
		t.Errorf("Take = %+v", got)
	}
	if _, err := s.Take("j1"); err != core.ErrNotFound {
		t.Errorf("second Take = %v, want core.ErrNotFound — a result must be delivered once", err)
	}
}

// TestResultTakeIsAtomicUnderRace is why Take removes and returns inside ONE
// critical section.
//
// Implemented as a read followed by a separate delete, two concurrent clients
// could both receive the same result and both be charged — and the second charge
// would look exactly like a legitimate one in the ledger.
func TestResultTakeIsAtomicUnderRace(t *testing.T) {
	s, _ := newStore(time.Hour)
	s.Put("j1", []string{"only"})

	const n = 32
	var (
		wins int64
		wg   sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Take("j1"); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d concurrent Takes produced %d winners, want exactly 1 — more than one "+
			"means the same result was delivered twice and charged twice", n, wins)
	}
}

func TestResultPeekDoesNotRemove(t *testing.T) {
	s, _ := newStore(time.Hour)
	s.Put("j1", []string{"x"})

	// Peek builds the backlog a reconnecting client is sent; it must not consume
	// anything, because the client has not fetched it yet.
	if _, ok := s.Peek("j1"); !ok {
		t.Fatal("first Peek did not find the result")
	}
	if _, ok := s.Peek("j1"); !ok {
		t.Error("second Peek did not find the result — Peek must not consume")
	}
	if _, err := s.Take("j1"); err != nil {
		t.Errorf("Take after two Peeks = %v, want success", err)
	}
}

func TestResultPeekMissing(t *testing.T) {
	s, _ := newStore(time.Hour)
	if _, ok := s.Peek("nope"); ok {
		t.Error("Peek of a missing job reported true")
	}
}

func TestResultExpires(t *testing.T) {
	s, c := newStore(30 * time.Minute)
	s.Put("j1", []string{"x"})

	c.advance(29 * time.Minute)
	if _, err := s.Take("j1"); err != nil {
		t.Fatalf("Take before expiry = %v, want success", err)
	}

	s.Put("j2", []string{"y"})
	c.advance(31 * time.Minute)
	if _, err := s.Take("j2"); err != core.ErrNotFound {
		t.Errorf("Take after expiry = %v, want core.ErrNotFound", err)
	}
}

func TestResultPeekRespectsExpiry(t *testing.T) {
	s, c := newStore(time.Minute)
	s.Put("j1", []string{"x"})
	c.advance(2 * time.Minute)
	if _, ok := s.Peek("j1"); ok {
		t.Error("Peek returned an expired result — a backlog must not offer a client " +
			"something a fetch would then refuse")
	}
}

// TestSweepReturnsExpiredIDs pins the contract that makes the TTL safe.
//
// The caller requeues the ids Sweep returns. A sweeper that dropped them
// silently would leave each job stranded in `done` with no result to serve,
// forever — a worse state than the memory leak the TTL exists to prevent, and an
// invisible one.
func TestSweepReturnsExpiredIDs(t *testing.T) {
	s, c := newStore(time.Hour)
	s.Put("old-1", []string{"a"})
	s.Put("old-2", []string{"b"})

	c.advance(2 * time.Hour)
	s.Put("fresh", []string{"c"})

	got := s.Sweep()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "old-1" || got[1] != "old-2" {
		t.Errorf("Sweep returned %v, want [old-1 old-2] — the ids ARE the contract, because "+
			"the caller requeues them", got)
	}
	if s.Len() != 1 {
		t.Errorf("Len after sweep = %d, want 1 (the fresh entry survives)", s.Len())
	}
	if _, ok := s.Peek("fresh"); !ok {
		t.Error("the unexpired entry was swept")
	}
}

func TestSweepIsExclusiveOfBoundary(t *testing.T) {
	s, c := newStore(time.Hour)
	s.Put("j1", []string{"x"})

	// Exactly at the expiry instant: treated as expired. Picking the boundary
	// deliberately, and asserting it, is what stops an off-by-one here becoming
	// an entry that never expires at all.
	c.advance(time.Hour)
	if got := s.Sweep(); len(got) != 1 {
		t.Errorf("an entry expiring exactly now was not swept (%v)", got)
	}

	s2, c2 := newStore(time.Hour)
	s2.Put("j2", []string{"x"})
	c2.advance(time.Hour - time.Second)
	if got := s2.Sweep(); len(got) != 0 {
		t.Errorf("an entry one second short of expiry was swept (%v)", got)
	}
}

func TestSweepEmpty(t *testing.T) {
	s, _ := newStore(time.Hour)
	if got := s.Sweep(); len(got) != 0 {
		t.Errorf("Sweep of an empty store = %v, want empty", got)
	}
}

func TestLenAndDrop(t *testing.T) {
	s, _ := newStore(time.Hour)
	s.Put("a", []string{"1"})
	s.Put("b", []string{"2"})
	if s.Len() != 2 {
		t.Errorf("Len = %d, want 2", s.Len())
	}
	s.Drop("a")
	if s.Len() != 1 {
		t.Errorf("Len after Drop = %d, want 1", s.Len())
	}
	if _, err := s.Take("a"); err != core.ErrNotFound {
		t.Errorf("Take after Drop = %v, want core.ErrNotFound", err)
	}
	s.Drop("nonexistent") // must not panic
}

func TestConcurrentPutTakeSweep(t *testing.T) {
	s, _ := newStore(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				id := core.NewID()
				s.Put(id, []string{"x"})
				_, _ = s.Take(id)
				_ = s.Sweep()
				_ = s.Len()
			}
		}(i)
	}
	wg.Wait()
}
