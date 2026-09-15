// Package ratelimit bounds how often one caller may be served.
//
// It is deliberately ignorant of HTTP, tokens and roles: it holds buckets keyed
// by an opaque string and decides with an instant the caller supplies. The
// keying policy — per bearer token, chosen in ADR-0002 because the threat is a
// leaked credential rather than a greedy customer — lives at the call site.
package ratelimit

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limit is one role's allowance.
//
// A zero Limit, or any non-positive RPS, means UNLIMITED. That is not a
// degenerate case to tolerate but the documented operational rollback: an
// operator sets --rate-client 0 and the limit is gone without a redeploy.
type Limit struct {
	// RPS is the sustained rate in requests per second.
	RPS float64
	// Burst is how many requests may arrive at once before the rate applies.
	Burst int
}

// unlimited reports whether this Limit disables limiting entirely.
func (l Limit) unlimited() bool { return l.RPS <= 0 || l.Burst <= 0 }

// entry is one key's bucket plus when it was last consulted.
type entry struct {
	bucket   *rate.Limiter
	lastSeen time.Time
}

// Limiter holds one token bucket per key.
//
// ⚠ THE MAP IS THE THING THAT CAN LEAK. One entry per key, never removed, is
// unbounded growth keyed on something an administrator can mint freely — so
// EvictIdle is not housekeeping, it is the reason this type can be used at all.
type Limiter struct {
	now  func() time.Time
	idle time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

// New returns a Limiter that decides with now() and forgets a key after idle.
//
// The clock is a field rather than a call to time.Now inside Allow, which is
// what lets every test advance time explicitly instead of sleeping. x/time/rate
// supports this through AllowN(t, n); the argument-less Allow() would silently
// reintroduce the wall clock, so it is never called here.
func New(now func() time.Time, idle time.Duration) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, idle: idle, entries: map[string]*entry{}}
}

// Allow reports whether this key may be served now, consuming a token if so.
func (l *Limiter) Allow(key string, lim Limit) bool {
	if lim.unlimited() {
		// No entry is created. "Disabled" that still grows the map would be a
		// leak wearing the shape of a switch.
		return true
	}
	now := l.now()

	l.mu.Lock()
	e := l.entryFor(key, lim, now)
	e.lastSeen = now
	l.mu.Unlock()

	return e.bucket.AllowN(now, 1)
}

// RetryAfter reports how long until this key's next token.
//
// It consumes nothing: asking when you may retry is not a retry. The middleware
// calls it immediately after a refusal to fill in Retry-After, and a version
// that took a token would make every 429 push its own deadline further out.
func (l *Limiter) RetryAfter(key string, lim Limit) time.Duration {
	if lim.unlimited() {
		return 0
	}
	now := l.now()

	l.mu.Lock()
	e := l.entryFor(key, lim, now)
	l.mu.Unlock()

	res := e.bucket.ReserveN(now, 1)
	if !res.OK() {
		// Unreachable with Burst >= 1, but returning the whole refill window is
		// the safe answer rather than zero, which would mean "retry now".
		return time.Duration(float64(time.Second) * float64(lim.Burst) / lim.RPS)
	}
	d := res.DelayFrom(now)
	res.CancelAt(now) // hand the token straight back
	return d
}

// entryFor returns the bucket for key, creating it on first use. Caller holds mu.
func (l *Limiter) entryFor(key string, lim Limit, now time.Time) *entry {
	e, ok := l.entries[key]
	if !ok {
		e = &entry{bucket: rate.NewLimiter(rate.Limit(lim.RPS), lim.Burst), lastSeen: now}
		l.entries[key] = e
	}
	return e
}

// EvictIdle drops every key untouched for longer than the idle window and
// returns how many were dropped.
//
// Dropping a key forgives its accumulated history, so a caller that idles past
// the window starts with a full burst again. That is deliberate: ten minutes of
// silence to reset a twenty-request burst is a far lower rate than the limit
// itself, and the alternative is a map that only ever grows.
func (l *Limiter) EvictIdle() int {
	cutoff := l.now().Add(-l.idle)

	l.mu.Lock()
	defer l.mu.Unlock()

	n := 0
	for k, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, k)
			n++
		}
	}
	return n
}

// Len reports how many keys are currently held.
//
// It exists so a test can assert the map SHRANK rather than that EvictIdle was
// called — the difference between checking the leak is fixed and checking that
// someone wrote a line of code about it.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// RetryAfterSeconds rounds a delay UP to whole seconds for the Retry-After
// header.
//
// Rounding down yields 0 for any sub-second wait, which tells a client to retry
// immediately into another refusal. The minimum this returns is 1.
func RetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int(math.Ceil(d.Seconds()))
}
