package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newCountingServer fronts the real API with a reader that counts what the
// HANDLER pulls off the wire, and reports that count when ServeHTTP returns.
//
// ⚠ The return-time snapshot is the whole point. net/http drains whatever a
// handler left unread so the connection can be reused, and a count taken after
// the client has its response includes that drain — which makes a correct
// streaming handler score identically to a buffering one.
func newCountingServer(t *testing.T, e *env, read *int64, done chan int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &countingBody{ReadCloser: r.Body, n: read}
		e.api.ServeHTTP(w, r)
		done <- atomic.LoadInt64(read)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding %s: %v", resp.Request.URL, err)
	}
}

func repeat(s string, n int) string { return strings.Repeat(s, n) }
