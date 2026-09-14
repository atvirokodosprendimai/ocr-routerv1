// Package bus is the router's in-process fan-out.
//
// It exists so a write path can say "this changed" without knowing who is
// listening, and so an SSE stream can listen without polling. With exactly one
// router process there is no second subscriber for NATS to reach, so this is
// the whole fan-out — but every publish and subscribe goes through this one
// interface, which is the seam where a cross-process implementation would go if
// the single-router decision is ever reversed.
package bus

import "sync"

// Kind names what happened. Deliberately few: an event is a nudge, not a
// payload, so the set only has to distinguish what a reader should DO next.
type Kind string

const (
	// KindWork tells workers serving a label that something is claimable.
	KindWork Kind = "work"
	// KindReady tells a client that a job's result is waiting.
	KindReady Kind = "ready"
	// KindFailed tells a client a job will not produce a result.
	KindFailed Kind = "failed"
	// KindAdmin tells the dashboard that something worth re-rendering moved.
	KindAdmin Kind = "admin"
)

// Event is a NOTIFICATION: ids and small scalars only.
//
// It deliberately carries no rendered state and no job output. The reader
// re-queries when it receives one, which is what makes two events racing for the
// same job resolve correctly — the second read wins and is current, whereas
// pushing rendered state would let an older render land last.
type Event struct {
	Kind Kind
	// JobID is empty for events that are about a queue rather than a job.
	JobID string
	// Units is the size of a ready result, so a client can show "3 pages" in its
	// backlog without a round trip. A count, never the content.
	Units int
	// Reason explains a failure — "expired", "attempts exhausted", a worker's
	// stderr tail.
	Reason string
	// Label is the service a work event concerns.
	Label string
}

// Topic names a fan-out channel.
//
// Client topics are per user so one customer's stream can never carry another's
// job ids. Worker topics are per LABEL so a strip-html worker is not woken by
// every OCR upload — with a fleet of several service types, a single shared
// worker topic would wake every process on every upload.
func UserTopic(userID string) string  { return "user:" + userID }
func WorkerTopic(label string) string { return "workers:" + label }

// AdminTopic is the single topic the dashboard subscribes to.
const AdminTopic = "admin"

// subscriber is one open stream.
type subscriber struct {
	ch   chan Event
	once sync.Once
}

// close shuts the channel exactly once.
//
// sync.Once matters because unsubscribe is reachable twice on a normal path — a
// deferred cleanup plus an error return — and closing a closed channel panics,
// which would take down the whole router for a cosmetic reason.
func (s *subscriber) close() {
	s.once.Do(func() { close(s.ch) })
}

// Bus fans events out to every subscriber of a topic, in this process.
type Bus struct {
	mu sync.Mutex
	// topics maps a topic to its subscriber set. The entry is deleted when the
	// last subscriber leaves, so a router serving a million users over its
	// lifetime does not accumulate a million empty maps.
	topics map[string]map[*subscriber]struct{}
}

// New returns an empty Bus.
func New() *Bus {
	return &Bus{topics: make(map[string]map[*subscriber]struct{})}
}

// Subscribe joins a topic and returns the channel and its cleanup.
//
// The channel is buffered to 1. The caller MUST call the returned function when
// it is done — a stream that returns without it leaks both the channel and the
// map entry.
func (b *Bus) Subscribe(topic string) (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, 1)}

	b.mu.Lock()
	if b.topics[topic] == nil {
		b.topics[topic] = make(map[*subscriber]struct{})
	}
	b.topics[topic][s] = struct{}{}
	b.mu.Unlock()

	return s.ch, func() { b.unsubscribe(topic, s) }
}

func (b *Bus) unsubscribe(topic string, s *subscriber) {
	b.mu.Lock()
	if set, ok := b.topics[topic]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(b.topics, topic)
		}
	}
	b.mu.Unlock()

	// Closed only AFTER the lock is released, and only once removal has
	// committed. Ordering with Publish (which sends while holding the lock) is
	// what makes this safe:
	//
	//   - Publish wins the lock first: it sends to this subscriber, finishes,
	//     unlocks; we then delete and close. No send follows the close.
	//   - We win first: we delete, unlock and close; Publish then acquires the
	//     lock and no longer sees this subscriber.
	//
	// Closing while still holding the lock would also be correct, but would mean
	// a channel close inside every publisher's critical section for no gain.
	s.close()
}

// Publish delivers e to every current subscriber of topic. It never blocks.
//
// The send is non-blocking with DROP-OLDEST: if a subscriber's buffer is full,
// the stale event is discarded and the new one takes its place. That is safe
// precisely because an Event is a notification — the reader re-queries on
// whatever it receives, so the newest nudge is strictly more useful than the
// one it replaced.
//
// ⚠ The direction matters and is easy to get backwards. Dropping the NEW event
// instead would leave a client looking at stale state until something else
// happened to move, which can be forever.
// ⚠ The sends happen while HOLDING the lock, and that is load-bearing rather
// than lazy. Copying the subscriber set out and sending after unlocking looks
// tidier and is a race: between the unlock and the send, unsubscribe can close
// the channel, and a send on a closed channel panics — taking down the router.
// Measured here on 2026-09-15 by TestConcurrentSubscribeUnsubscribePublish,
// which panicked on exactly that ordering.
//
// Holding a mutex across a send is normally the thing to avoid, but every send
// below is non-blocking, so the critical section is a bounded sequence of
// constant-time operations and no publisher can be parked in it.
func (b *Bus) Publish(topic string, e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for s := range b.topics[topic] {
		select {
		case s.ch <- e:
		default:
			// Buffer full: discard the superseded event, then deliver this one.
			// Both steps are non-blocking, so a reader draining concurrently
			// cannot wedge the publisher here.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- e:
			default:
			}
		}
	}
}

// Subscribers reports how many streams are currently on a topic.
//
// This is what makes "no worker is serving this label" observable, which is the
// silent failure the whole monitoring story exists to surface.
func (b *Bus) Subscribers(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.topics[topic])
}

// Topics returns every topic with at least one subscriber.
//
// Live worker topics are what the label registry is derived from: a service is
// available because a worker is serving it, not because someone filled in a
// form.
func (b *Bus) Topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.topics))
	for t := range b.topics {
		out = append(out, t)
	}
	return out
}
