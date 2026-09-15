package ratelimit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/ratelimit"
)

var base = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// clock is a hand-advanced clock. Every test here moves time explicitly; none
// sleeps, which is what the injected-clock design buys.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: base} }

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

func TestAllowsBurstThenRefuses(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 1, Burst: 3}

	for i := 0; i < 3; i++ {
		if !l.Allow("k", lim) {
			t.Fatalf("call %d of the burst was refused", i+1)
		}
	}
	if l.Allow("k", lim) {
		t.Error("the call past the burst was allowed — nothing is being limited")
	}
}

func TestRefillsOverTime(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 2, Burst: 3} // one token every 500ms

	for i := 0; i < 3; i++ {
		l.Allow("k", lim)
	}

	c.advance(500 * time.Millisecond)
	if !l.Allow("k", lim) {
		t.Fatal("no token after one refill interval — the rate is wrong, not merely the burst")
	}
	// ★ Exactly one: a second immediately after must fail, or the test would pass
	// against an implementation that refills the whole bucket on any elapsed time.
	if l.Allow("k", lim) {
		t.Error("two tokens arrived after one refill interval")
	}
}

// TestClockIsInjectedNotWallClock pins a frozen clock and demands refusal.
//
// ⚠ The assertion is deliberately REFUSAL rather than eventual success. A test
// that waited for an allow would pass against a wall-clock implementation as
// real time passed; this one goes red for exactly that implementation, because
// real seconds elapse during the loop and would refill a wall-clock bucket.
func TestClockIsInjectedNotWallClock(t *testing.T) {
	c := newClock() // never advanced
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 1000, Burst: 2}

	l.Allow("k", lim)
	l.Allow("k", lim)

	for i := 0; i < 200; i++ {
		if l.Allow("k", lim) {
			t.Fatalf("call %d was allowed under a frozen clock — a call site is reading "+
				"time.Now() directly, which makes this package untestable and inconsistent", i)
		}
		time.Sleep(time.Millisecond) // real time passes; the injected clock does not
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 1, Burst: 2}

	l.Allow("a", lim)
	l.Allow("a", lim)
	if l.Allow("a", lim) {
		t.Fatal("key a was not exhausted, so this test proves nothing")
	}
	if !l.Allow("b", lim) {
		t.Error("exhausting key a refused key b — every caller shares one bucket")
	}
}

func TestZeroLimitIsUnlimited(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)

	for _, lim := range []ratelimit.Limit{{}, {RPS: 0, Burst: 100}, {RPS: -1, Burst: 5}} {
		for i := 0; i < 500; i++ {
			if !l.Allow("k", lim) {
				t.Fatalf("%+v refused call %d — the documented rollback switch does not work", lim, i)
			}
		}
	}
}

func TestZeroLimitCreatesNoEntry(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)

	for i := 0; i < 1000; i++ {
		l.Allow("key", ratelimit.Limit{})
	}
	if got := l.Len(); got != 0 {
		t.Errorf("Len = %d with limiting disabled, want 0 — 'disabled' still leaks one entry "+
			"per key, which is the memory leak wearing the shape of a switch", got)
	}
}

func TestRetryAfterIsPositiveWhenRefused(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 2, Burst: 2} // 500ms per token

	l.Allow("k", lim)
	l.Allow("k", lim)
	if l.Allow("k", lim) {
		t.Fatal("not refused, so there is nothing to retry after")
	}

	d := l.RetryAfter("k", lim)
	if d <= 0 {
		t.Fatalf("RetryAfter = %v after a refusal, want > 0", d)
	}
	if max := time.Second; d > max {
		t.Errorf("RetryAfter = %v, want no more than burst/rps = %v", d, max)
	}
}

func TestRetryAfterConsumesNothing(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Hour)
	lim := ratelimit.Limit{RPS: 1, Burst: 5}

	for i := 0; i < 10; i++ {
		l.RetryAfter("k", lim)
	}
	for i := 0; i < 5; i++ {
		if !l.Allow("k", lim) {
			t.Fatalf("only %d of 5 tokens survived ten RetryAfter calls — asking when to retry "+
				"is being charged as a retry, so every 429 pushes its own deadline further out", i)
		}
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want int
	}{
		{1 * time.Millisecond, 1},
		{999 * time.Millisecond, 1},
		{1001 * time.Millisecond, 2},
		{0, 1},
		{-time.Second, 1},
	} {
		if got := ratelimit.RetryAfterSeconds(tc.in); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d — rounding down yields 0 and tells a "+
				"client to retry straight into another refusal", tc.in, got, tc.want)
		}
	}
}

// TestEvictIdleShrinksTheMap asserts the map SHRANK.
//
// Asserting that EvictIdle was called proves that someone wrote a line of code
// about the leak, not that the leak is fixed.
func TestEvictIdleShrinksTheMap(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, 10*time.Minute)
	lim := ratelimit.Limit{RPS: 100, Burst: 100}

	for i := 0; i < 100; i++ {
		l.Allow(string(rune('a'+i%26))+string(rune('0'+i/26)), lim)
	}
	if got := l.Len(); got != 100 {
		t.Fatalf("Len = %d before eviction, want 100 — the fixture never filled the map", got)
	}

	c.advance(11 * time.Minute)
	if n := l.EvictIdle(); n != 100 {
		t.Errorf("EvictIdle dropped %d, want 100", n)
	}
	if got := l.Len(); got != 0 {
		t.Errorf("Len = %d after eviction, want 0 — the map did not shrink", got)
	}
}

// TestEvictIdleKeepsActiveKeys is the necessary companion to the test above,
// which passes on its own against an implementation that clears everything.
func TestEvictIdleKeepsActiveKeys(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, 10*time.Minute)
	lim := ratelimit.Limit{RPS: 100, Burst: 100}

	l.Allow("stale", lim)
	c.advance(11 * time.Minute)
	l.Allow("fresh", lim)

	if n := l.EvictIdle(); n != 1 {
		t.Errorf("EvictIdle dropped %d, want exactly 1 — an active caller was forgotten", n)
	}
	if got := l.Len(); got != 1 {
		t.Errorf("Len = %d, want 1 (the fresh key)", got)
	}
}

func TestEvictedKeyStartsFresh(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, 10*time.Minute)
	lim := ratelimit.Limit{RPS: 1, Burst: 2}

	l.Allow("k", lim)
	l.Allow("k", lim)

	c.advance(11 * time.Minute)
	l.EvictIdle()

	// Deliberate forgiveness (ADR-0002 Risk 2), written down so it is not later
	// read as a bug: eleven minutes of silence to reset a two-request burst is a
	// far lower rate than the limit itself.
	// Both calls are made unconditionally: `a() || b()` short-circuits, so a
	// failure on the first would leave the second never exercised.
	for i := 0; i < lim.Burst; i++ {
		if !l.Allow("k", lim) {
			t.Errorf("call %d after eviction was refused — an evicted key did not get a full "+
				"burst back", i+1)
		}
	}
}

func TestConcurrentAllowIsRaceFree(t *testing.T) {
	c := newClock()
	l := ratelimit.New(c.now, time.Minute)
	lim := ratelimit.Limit{RPS: 1000, Burst: 1000}

	stop := make(chan struct{})
	var evicting sync.WaitGroup
	evicting.Add(1)
	go func() {
		defer evicting.Done()
		for {
			select {
			case <-stop:
				return
			default:
				l.EvictIdle()
				l.Len()
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := string(rune('a' + g%10))
			for i := 0; i < 100; i++ {
				l.Allow(key, lim)
				l.RetryAfter(key, lim)
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	evicting.Wait()

	// -race is the primary verdict here, but a test whose only failure mode is a
	// flag on the command line goes green when someone drops the flag. These
	// assertions give it a way to fail on its own: concurrent creation under a
	// check-then-act path can lose an entry or duplicate one, and either shows up
	// as a count outside 0..10.
	if got := l.Len(); got < 0 || got > 10 {
		t.Errorf("Len = %d after concurrent use of 10 keys, want 0..10 — entries were lost or "+
			"duplicated by racing creation", got)
	}
	if !l.Allow("a", lim) {
		t.Error("the limiter stopped serving after concurrent use")
	}
}
