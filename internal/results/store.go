// Package results holds finished job output in memory, and only in memory.
//
// A result never touches disk. The operator's rule is that results are deleted
// once transferred, and the source blob on disk already makes the work
// recoverable — so an abandoned or lost result costs repeated work and never a
// lost document, and because nothing is charged before delivery it can never
// cost a double charge either.
package results

import (
	"sync"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

type entry struct {
	result    core.Result
	expiresAt time.Time
}

// Store is a TTL map of job id to result.
type Store struct {
	mu  sync.Mutex
	m   map[string]entry
	ttl time.Duration
	// now is injected so the TTL tests assert expiry deterministically instead
	// of sleeping. A test that sleeps is slow and flaky; a test that moves the
	// clock is neither.
	now func() time.Time
}

// New returns a Store whose entries expire after ttl.
func New(ttl time.Duration) *Store {
	return &Store{m: make(map[string]entry), ttl: ttl, now: time.Now}
}

// SetClock replaces the time source. Tests only.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Put stores a result, stamping its expiry.
func (s *Store) Put(jobID string, units []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[jobID] = entry{
		result:    core.Result{JobID: jobID, Units: units},
		expiresAt: s.now().Add(s.ttl),
	}
}

// Take returns a result and removes it, in ONE critical section.
//
// The atomicity is the point. Implemented as a read followed by a separate
// delete, two concurrent clients could both receive the same result and both be
// charged for it — and the second charge would look exactly like a legitimate
// one in the ledger.
func (s *Store) Take(jobID string) (core.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.m[jobID]
	if !ok {
		return core.Result{}, core.ErrNotFound
	}
	delete(s.m, jobID)
	if !e.expiresAt.After(s.now()) {
		// Expired between being stored and being taken. Treat it as gone rather
		// than serving it: the sweeper has either already requeued the job or is
		// about to, and serving a result whose job is queued again would charge
		// for work that is about to be redone.
		return core.Result{}, core.ErrNotFound
	}
	return e.result, nil
}

// Peek reports whether a live result exists, without removing it.
//
// Used to build the backlog a reconnecting client is sent, which must not
// consume anything: the client has not fetched it yet.
func (s *Store) Peek(jobID string) (core.Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[jobID]
	if !ok || !e.expiresAt.After(s.now()) {
		return core.Result{}, false
	}
	return e.result, true
}

// Sweep removes every expired entry and RETURNS THEIR JOB IDS.
//
// Returning the ids is not a convenience — it is the contract. The caller
// requeues those jobs, and a sweeper that silently dropped them would leave each
// job stranded in `done` with no result to serve, forever. That is a worse state
// than the memory leak the TTL exists to prevent, and an invisible one.
func (s *Store) Sweep() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var expired []string
	for id, e := range s.m {
		if !e.expiresAt.After(now) {
			expired = append(expired, id)
			delete(s.m, id)
		}
	}
	return expired
}

// Len reports how many results are held. Exported so the dashboard and the
// metrics endpoint can show the unbounded-growth risk rather than guess at it.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Drop removes a result without returning it, for a job that has died.
func (s *Store) Drop(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, jobID)
}
