package bus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
)

// recv waits briefly for one event. Every wait in this file is bounded so a
// broken fan-out fails the test rather than hanging the suite.
func recv(t *testing.T, ch <-chan bus.Event) (bus.Event, bool) {
	t.Helper()
	select {
	case e, ok := <-ch:
		return e, ok
	case <-time.After(2 * time.Second):
		return bus.Event{}, false
	}
}

func TestPublishReachesAllSubscribers(t *testing.T) {
	b := bus.New()
	a, closeA := b.Subscribe("t")
	defer closeA()
	c, closeC := b.Subscribe("t")
	defer closeC()

	want := bus.Event{Kind: bus.KindReady, JobID: "j1", Units: 7, Reason: "", Label: "ocr"}
	b.Publish("t", want)

	for i, ch := range []<-chan bus.Event{a, c} {
		got, ok := recv(t, ch)
		if !ok {
			t.Fatalf("subscriber %d received nothing", i)
		}
		if got != want {
			t.Errorf("subscriber %d got %+v, want %+v — every Event field must survive the "+
				"fan-out intact", i, got, want)
		}
	}
}

func TestPublishIsTopicScoped(t *testing.T) {
	b := bus.New()
	a, closeA := b.Subscribe(bus.UserTopic("customer-a"))
	defer closeA()

	b.Publish(bus.UserTopic("customer-b"), bus.Event{Kind: bus.KindReady, JobID: "b-secret"})

	select {
	case e := <-a:
		t.Fatalf("customer A received %+v published to customer B — per-user topics are what "+
			"stop one customer seeing another's job ids", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWorkerTopicsAreLabelScoped(t *testing.T) {
	b := bus.New()
	ocr, closeOCR := b.Subscribe(bus.WorkerTopic("ocr"))
	defer closeOCR()

	b.Publish(bus.WorkerTopic("strip-html"), bus.Event{Kind: bus.KindWork, Label: "strip-html"})

	select {
	case e := <-ocr:
		t.Fatalf("an ocr worker was woken by %+v on the strip-html topic — collapsing worker "+
			"topics back to one shared topic wakes every process in the fleet on every upload", e)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := bus.New()
	// A subscriber that never reads. Its buffer fills after one event.
	_, closeStalled := b.Subscribe("t")
	defer closeStalled()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish("t", bus.Event{Kind: bus.KindWork})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads — one stalled browser would " +
			"stall every write in the router")
	}
}

// TestDropOldestKeepsLatest pins the DIRECTION of the drop.
//
// Dropping the newest on a full buffer is equally easy to write and leaves a
// client looking at stale state until something else happens to move — possibly
// forever. Asserting which of the two events survives is the only thing that
// separates the correct implementation from the harmful one.
func TestDropOldestKeepsLatest(t *testing.T) {
	b := bus.New()
	ch, done := b.Subscribe("t")
	defer done()

	b.Publish("t", bus.Event{Kind: bus.KindReady, JobID: "first"})
	b.Publish("t", bus.Event{Kind: bus.KindReady, JobID: "second"})

	got, ok := recv(t, ch)
	if !ok {
		t.Fatal("received nothing after two publishes")
	}
	if got.JobID != "second" {
		t.Errorf("received %q, want %q — a full buffer must drop the SUPERSEDED event, not the "+
			"new one; dropping the new one strands the reader on stale state", got.JobID, "second")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := bus.New()
	ch, done := b.Subscribe("t")
	done()

	b.Publish("t", bus.Event{Kind: bus.KindReady, JobID: "after-unsubscribe"})

	e, ok := <-ch
	if ok {
		t.Errorf("channel delivered %+v after unsubscribe; it should be closed", e)
	}
}

func TestUnsubscribeTwiceDoesNotPanic(t *testing.T) {
	b := bus.New()
	ch, done := b.Subscribe("t")

	// Reachable on a normal path: a deferred cleanup plus an explicit call on an
	// error return. Closing a closed channel panics, which would take the router
	// down for a cosmetic reason.
	//
	// The recover is what gives this test a reachable failure call. Relying on
	// "the test panics if it is broken" works, but leaves a body in which no
	// assertion can fire — indistinguishable, to anything reading the file, from
	// a test that checks nothing.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second unsubscribe panicked: %v", r)
			}
		}()
		done()
		done()
	}()

	if _, open := <-ch; open {
		t.Error("channel still open after unsubscribe")
	}
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers = %d after two unsubscribes, want 0", got)
	}
}

func TestEmptyTopicIsReaped(t *testing.T) {
	b := bus.New()
	_, a := b.Subscribe("t")
	_, c := b.Subscribe("t")

	if got := b.Subscribers("t"); got != 2 {
		t.Fatalf("Subscribers = %d, want 2", got)
	}
	a()
	if got := b.Subscribers("t"); got != 1 {
		t.Errorf("Subscribers after one unsubscribe = %d, want 1", got)
	}
	c()
	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers after all unsubscribed = %d, want 0", got)
	}
	for _, topic := range b.Topics() {
		if topic == "t" {
			t.Error("the empty topic is still in the map — a long-lived router would accumulate " +
				"one empty map per user it has ever served")
		}
	}
}

func TestTopicsListsLiveTopicsOnly(t *testing.T) {
	b := bus.New()
	_, closeOCR := b.Subscribe(bus.WorkerTopic("ocr"))
	defer closeOCR()
	_, closeCrawl := b.Subscribe(bus.WorkerTopic("crawl"))

	topics := b.Topics()
	if len(topics) != 2 {
		t.Fatalf("Topics = %v, want 2", topics)
	}
	closeCrawl()

	topics = b.Topics()
	if len(topics) != 1 || topics[0] != bus.WorkerTopic("ocr") {
		t.Errorf("Topics after the crawl worker left = %v, want only the ocr topic — the live "+
			"label registry is derived from exactly this", topics)
	}
}

func TestConcurrentSubscribeUnsubscribePublish(t *testing.T) {
	b := bus.New()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publishers hammering the topic while subscribers come and go. Under -race
	// this is what catches bookkeeping done outside the mutex.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish("t", bus.Event{Kind: bus.KindWork})
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ch, done := b.Subscribe("t")
				select {
				case <-ch:
				default:
				}
				done()
			}
		}()
	}

	subDone := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(subDone)
	}()
	<-subDone
	close(stop)
	wg.Wait()

	if got := b.Subscribers("t"); got != 0 {
		t.Errorf("Subscribers after every subscriber left = %d, want 0", got)
	}
}

func TestTopicHelpers(t *testing.T) {
	if got := bus.UserTopic("u1"); got != "user:u1" {
		t.Errorf("UserTopic = %q", got)
	}
	if got := bus.WorkerTopic("ocr"); got != "workers:ocr" {
		t.Errorf("WorkerTopic = %q", got)
	}
	// A user id must never be able to collide with a worker topic.
	if bus.UserTopic("s:ocr") == bus.WorkerTopic("ocr") {
		t.Error("a user topic collided with a worker topic")
	}
}
