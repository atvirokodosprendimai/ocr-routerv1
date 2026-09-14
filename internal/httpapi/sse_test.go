package httpapi_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
)

// readEvents opens an SSE stream and returns a channel of "event" names plus a
// cancel. Every read is bounded so a broken stream fails rather than hangs.
func readEvents(t *testing.T, url, token string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("opening stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	out := make(chan string, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				select {
				case out <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, cancel
}

func waitFor(t *testing.T, ch <-chan string, want string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %q", want)
			}
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out after %v waiting for event %q", within, want)
		}
	}
}

func TestSSESendsHelloAndBacklog(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	defer cancel()

	waitFor(t, events, "hello", 2*time.Second)
	waitFor(t, events, "backlog", 2*time.Second)
}

func TestSSEDeliversReadyEvent(t *testing.T) {
	e := newEnv(t)
	e.liveWorker(t, "ocr")

	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	defer cancel()
	waitFor(t, events, "hello", 2*time.Second)

	// Publishing on the user's topic is what a finished job does.
	e.bus.Publish(bus.UserTopic(e.clientID), bus.Event{Kind: bus.KindReady, JobID: "j1", Units: 3})
	waitFor(t, events, "ready", 2*time.Second)
}

func TestSSEIsUserScoped(t *testing.T) {
	e := newEnv(t)
	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	defer cancel()
	waitFor(t, events, "hello", 2*time.Second)

	// Another customer's event must never appear on this stream.
	e.bus.Publish(bus.UserTopic("somebody-else"), bus.Event{Kind: bus.KindReady, JobID: "not-yours"})

	select {
	case got := <-events:
		if got == "ready" {
			t.Fatal("a customer's stream carried another customer's event")
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSSEWorkerStreamIsLabelScoped(t *testing.T) {
	e := newEnv(t)

	events, cancel := readEvents(t, e.srv.URL+"/sse?label=ocr", e.workerTok)
	defer cancel()
	waitFor(t, events, "hello", 2*time.Second)

	e.bus.Publish(bus.WorkerTopic("strip-html"), bus.Event{Kind: bus.KindWork, Label: "strip-html"})
	select {
	case got := <-events:
		if got == "work" {
			t.Fatal("an ocr worker stream carried a strip-html work event")
		}
	case <-time.After(300 * time.Millisecond):
	}

	e.bus.Publish(bus.WorkerTopic("ocr"), bus.Event{Kind: bus.KindWork, Label: "ocr"})
	waitFor(t, events, "work", 2*time.Second)
}

func TestSSEWorkerRequiresLabel(t *testing.T) {
	e := newEnv(t)
	resp := e.do(t, "GET", "/sse", e.workerTok, nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("worker stream without ?label = %d, want 400 — otherwise it would subscribe "+
			"to nothing and wait forever", resp.StatusCode)
	}
}

func TestSSESendsPing(t *testing.T) {
	e := newEnv(t) // PingInterval is 50ms in the test env
	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	defer cancel()
	waitFor(t, events, "ping", 3*time.Second)
}

func TestSSEUnsubscribesOnDisconnect(t *testing.T) {
	e := newEnv(t)
	topic := bus.UserTopic(e.clientID)

	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	waitFor(t, events, "hello", 2*time.Second)
	if got := e.bus.Subscribers(topic); got != 1 {
		t.Fatalf("Subscribers while streaming = %d, want 1", got)
	}

	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.bus.Subscribers(topic) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Subscribers = %d after the client disconnected, want 0 — a stream that does not "+
		"unsubscribe leaks a channel and a map entry per connection", e.bus.Subscribers(topic))
}

// TestSSEClearsWriteDeadline is the only test here that can catch a whole class
// of silent production failure.
//
// ⚠ It is also easy to write so that it CANNOT fail: against a server with no
// WriteTimeout it passes with the deadline-clearing line deleted. The server
// below therefore sets a deliberately short WriteTimeout, and the test asserts
// that a stream still delivers an event well past it.
func TestSSEClearsWriteDeadline(t *testing.T) {
	e := newEnv(t)

	// A server whose WriteTimeout would guillotine any stream after 150ms.
	srv := httptest.NewUnstartedServer(e.api)
	srv.Config.WriteTimeout = 150 * time.Millisecond
	srv.Start()
	defer srv.Close()

	events, cancel := readEvents(t, srv.URL+"/sse", e.clientTok)
	defer cancel()
	waitFor(t, events, "hello", 2*time.Second)

	// Well past the WriteTimeout. Without the per-stream deadline clear, the
	// connection is dead by now and this event never arrives.
	time.Sleep(400 * time.Millisecond)
	e.bus.Publish(bus.UserTopic(e.clientID), bus.Event{Kind: bus.KindReady, JobID: "late"})

	waitFor(t, events, "ready", 3*time.Second)
}

func TestSSEStopsWhenContextCancelled(t *testing.T) {
	e := newEnv(t)
	events, cancel := readEvents(t, e.srv.URL+"/sse", e.clientTok)
	waitFor(t, events, "hello", 2*time.Second)
	cancel()

	// The channel must close rather than hang: the handler returns on
	// r.Context().Done().
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the stream did not end after the client cancelled")
		}
	}
}
