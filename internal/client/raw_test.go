package client_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

const pngMagic = "\x89PNG\r\n\x1a\n"

// rawRouter is a fake router that answers /files/{id} with the given content
// type and body, and records what /upload was asked for.
func rawRouter(t *testing.T, ct, body string, gotRaw *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprint(w, "event: hello\ndata: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(50 * time.Millisecond)
			fmt.Fprint(w, "event: ready\ndata: {\"job_id\":\"job-1\"}\n\n")
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
		case r.URL.Path == "/upload":
			*gotRaw = r.URL.Query().Get("raw")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
		default:
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRawFlagReachesUploadQuery is the rung-2 selection for `--raw`.
//
// The units case is asserted too: without it, the raw case is satisfied by a
// client that hardcodes raw=1.
func TestRawFlagReachesUploadQuery(t *testing.T) {
	for _, c := range []struct {
		raw  bool
		want string
	}{{true, "1"}, {false, "0"}} {
		var gotRaw string
		body := `{"job_id":"job-1","units":["a"]}`
		ct := "application/json"
		if c.raw {
			body, ct = pngMagic, "application/octet-stream"
		}
		srv := rawRouter(t, ct, body, &gotRaw)

		cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
		if _, err := client.Submit(ctx(t), cfg,
			client.Input{Body: strings.NewReader("doc"), Raw: c.raw}, nil); err != nil {
			t.Fatalf("Submit(raw=%v): %v", c.raw, err)
		}
		if gotRaw != c.want {
			t.Errorf("Raw=%v produced ?raw=%q, want %q — sending it explicitly in BOTH modes is "+
				"what makes a mismatch a disagreement between two stated positions rather than "+
				"between a statement and a default", c.raw, gotRaw, c.want)
		}
	}
}

// TestCollectReadsOctetStreamAsBytes: an octet-stream response becomes bytes.
func TestCollectReadsOctetStreamAsBytes(t *testing.T) {
	var gotRaw string
	srv := rawRouter(t, "application/octet-stream", pngMagic, &gotRaw)

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	res, err := client.Submit(ctx(t), cfg,
		client.Input{Body: strings.NewReader("doc"), Raw: true}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if string(res.Raw) != pngMagic {
		t.Errorf("collected % x (%d bytes), want % x (%d bytes)",
			res.Raw, len(res.Raw), pngMagic, len(pngMagic))
	}
	if len(res.Units) != 0 {
		t.Errorf("a raw result also produced %d unit(s)", len(res.Units))
	}
}

// TestCollectBranchesOnResponseNotRequest keeps a disagreement visible.
//
// The client asked for raw and the server answered with units. Branching on the
// REQUEST would read a JSON envelope as bytes; branching on the RESPONSE reports
// what actually arrived, which is the case worth seeing.
func TestCollectBranchesOnResponseNotRequest(t *testing.T) {
	var gotRaw string
	srv := rawRouter(t, "application/json", `{"job_id":"job-1","units":["a"]}`, &gotRaw)

	cfg := client.Config{RouterURL: srv.URL, Token: "t", HTTP: srv.Client()}
	res, err := client.Submit(ctx(t), cfg,
		client.Input{Body: strings.NewReader("doc"), Raw: true}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(res.Raw) != 0 {
		t.Error("a JSON response was read as raw bytes — the client branched on its own request")
	}
	if len(res.Units) != 1 || res.Units[0] != "a" {
		t.Errorf("units = %v, want [a]", res.Units)
	}
}
