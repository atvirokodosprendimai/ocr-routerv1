package httpapi_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
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

// countSuffix counts files under dir whose name ends in suffix.
//
// It walks the real directory rather than asking the store, because the property
// under test is that a FILE is gone — and a count taken from the component that
// was supposed to delete it would only ever agree with itself.
func countSuffix(t *testing.T, dir, suffix string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, suffix) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return n
}

// mintWorker creates a SECOND worker, so a test can send a request from a
// principal that holds no lease on the job it names.
//
// One worker token cannot exercise the lease guard: it is the holder, so every
// call it makes is authorised and the check is never reached.
func (e *env) mintWorker(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()
	ap, err := e.ident.Authenticate(ctx, "Bearer "+e.adminTok, base)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	u, err := e.ident.CreateUser(ctx, ap, email, core.RoleWorker, base)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	_, tok, err := e.ident.MintToken(ctx, ap, u.ID, core.RoleWorker, "w2", base)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	return tok
}
