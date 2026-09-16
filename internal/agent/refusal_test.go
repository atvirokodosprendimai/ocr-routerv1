package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/agent"
	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// refusingRouter answers every subscribe with one status and body, and counts
// the attempts.
func refusingRouter(t *testing.T, code int, body string, attempts *atomic.Int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sse" {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runAgentAgainst(t *testing.T, url string) <-chan error {
	t.Helper()
	a := agent.New(agent.Config{
		RouterURL: url, Token: "t", Label: "shit", TmpDir: t.TempDir(), Slots: 1,
		Raw: true,
	}, runner.Runner{Cmd: "/bin/true", Timeout: time.Second, MaxOutput: 1 << 10})
	a.Log = func(string, ...any) {}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return done
}

// TestModeMismatchStopsTheWorker is the behaviour ADR-0006's risk table promised
// and the first implementation did not deliver.
//
// Observed 2026-09-16 against a running router: a worker started `--raw` on a
// service the administrator had not marked raw logged "stream ended (stream
// returned 409 Conflict); reconnecting in 1s" and did that forever. The fix is
// one checkbox, and nothing in the loop said which checkbox or why.
func TestModeMismatchStopsTheWorker(t *testing.T) {
	var attempts atomic.Int64
	url := refusingRouter(t, http.StatusConflict,
		`{"error":"output mode mismatch: worker declares raw for \"shit\", which the operator has configured as units"}`,
		&attempts)

	select {
	case err := <-runAgentAgainst(t, url):
		if err == nil {
			t.Fatal("the worker exited CLEANLY on a mode mismatch — an operator scripting around " +
				"this would see success for a worker that served nothing")
		}
		// The router's own explanation must survive to the operator, or the
		// message is "409 Conflict" and the fix is unguessable.
		if !strings.Contains(err.Error(), "configured as units") {
			t.Errorf("the error lost the router's explanation: %v", err)
		}
		if !strings.Contains(err.Error(), "shit") {
			t.Errorf("the error does not name the label: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the worker was still retrying after 5s (%d attempts) — a configuration refusal "+
			"answers identically every time, so the loop never ends", attempts.Load())
	}

	if n := attempts.Load(); n != 1 {
		t.Errorf("the worker made %d subscribe attempts, want 1 — a retry cannot change this answer", n)
	}
}

// TestRateLimitIsStillRetried is why this is not "4xx is fatal".
//
// 429 is explicitly temporary and backing off is the correct response; treating
// it as fatal would kill a worker for being briefly busy.
func TestRateLimitIsStillRetried(t *testing.T) {
	var attempts atomic.Int64
	url := refusingRouter(t, http.StatusTooManyRequests, `{"error":"rate limited"}`, &attempts)

	select {
	case err := <-runAgentAgainst(t, url):
		t.Fatalf("the worker gave up on a 429 after %d attempt(s) (%v) — rate limiting is "+
			"temporary and retrying is what it asks for", attempts.Load(), err)
	case <-time.After(2500 * time.Millisecond):
		if attempts.Load() < 2 {
			t.Errorf("the worker made %d subscribe attempt(s) in 2.5s, want at least 2 — a 429 "+
				"must still be retried", attempts.Load())
		}
	}
}

// TestServerErrorIsStillRetried: a router that fell over may come back, which is
// what the reconnect loop exists for.
func TestServerErrorIsStillRetried(t *testing.T) {
	var attempts atomic.Int64
	url := refusingRouter(t, http.StatusBadGateway, "upstream is down", &attempts)

	select {
	case err := <-runAgentAgainst(t, url):
		t.Fatalf("the worker gave up on a 502 (%v) — the router may come back", err)
	case <-time.After(2500 * time.Millisecond):
		if attempts.Load() < 2 {
			t.Errorf("the worker made %d attempt(s) in 2.5s, want at least 2", attempts.Load())
		}
	}
}
