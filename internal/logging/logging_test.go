package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/logging"
)

// TestJobLogsParamKeysAndNeverValues is the most important assertion in this
// package.
//
// ⚠ It asserts the KEY IS PRESENT as well as that the secret is absent. A test
// that only checks for absence passes when the implementation logs nothing at
// all, when the fixture has no params, and when the sentinel is misspelled —
// three ways to be reassured by a test that proves nothing.
func TestJobLogsParamKeysAndNeverValues(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Out: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	params := map[string]string{
		"url":   "https://alice:hunter2@internal.example.com/private/report",
		"depth": "3",
	}
	log.Info("job accepted", logging.Job("job-1", "user-1", "crawl", params))

	line := buf.String()
	if line == "" {
		t.Fatal("nothing was logged, so neither half of this test proves anything")
	}
	for _, want := range []string{"url", "depth", "job-1", "user-1", "crawl"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not contain %q, so the absence checks below are vacuous:\n%s",
				want, line)
		}
	}
	for _, secret := range []string{"hunter2", "alice", "internal.example.com", "private/report"} {
		if strings.Contains(line, secret) {
			t.Errorf("the log line leaks %q from a customer-supplied param value:\n%s", secret, line)
		}
	}
}

func TestJobWithNoParamsStillLogsTheJob(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logging.New(logging.Options{Out: &buf})
	log.Info("job accepted", logging.Job("job-2", "user-2", "ocr", nil))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, buf.String())
	}
	job, ok := rec["job"].(map[string]any)
	if !ok {
		t.Fatalf("no job group: %s", buf.String())
	}
	// The shape stays constant so a log query can rely on it: an absent attribute
	// and an empty one are different things to anything parsing this.
	if _, ok := job["params"]; !ok {
		t.Error("the params attribute is missing entirely with no params, rather than empty")
	}
	if job["id"] != "job-2" {
		t.Errorf("job.id = %v", job["id"])
	}
}

func TestParamKeysAreSorted(t *testing.T) {
	// Go randomises map iteration, so a single pass can pass by luck. Twenty
	// passes over keys whose insertion order differs from their sorted order
	// makes an unsorted implementation fail essentially always.
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		log, _ := logging.New(logging.Options{Out: &buf})
		log.Info("x", logging.Job("j", "u", "l", map[string]string{
			"zebra": "1", "alpha": "2", "mango": "3",
		}))

		var rec struct {
			Job struct {
				Params []string `json:"params"`
			} `json:"job"`
		}
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		want := []string{"alpha", "mango", "zebra"}
		if len(rec.Job.Params) != 3 {
			t.Fatalf("params = %v, want 3 keys", rec.Job.Params)
		}
		for j := range want {
			if rec.Job.Params[j] != want[j] {
				t.Fatalf("pass %d: params = %v, want %v — unsorted keys make two runs of the "+
					"same job undiffable", i, rec.Job.Params, want)
			}
		}
	}
}

func TestOutputIsJSONByDefault(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logging.New(logging.Options{Out: &buf})
	log.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("default output is not JSON: %v (%s)", err, buf.String())
	}
	for _, k := range []string{"level", "msg", "time"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("missing %q key", k)
		}
	}
}

func TestTextFormatIsAvailable(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Format: "text", Out: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("hello", "k", "v")

	out := buf.String()
	if json.Valid(buf.Bytes()) {
		t.Errorf("text format emitted JSON: %s", out)
	}
	if !strings.Contains(out, "k=v") {
		t.Errorf("text format did not emit key=value: %s", out)
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	log, err := logging.New(logging.Options{Level: "warn", Out: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	log.Info("this-is-info")
	if strings.Contains(buf.String(), "this-is-info") {
		t.Error("an info line survived a warn-level filter — the filter does not filter")
	}
	log.Warn("this-is-warn")
	if !strings.Contains(buf.String(), "this-is-warn") {
		t.Error("a warn line was dropped at warn level — the filter drops everything, which " +
			"passes the assertion above without doing anything useful")
	}
}

func TestUnknownLevelIsAnError(t *testing.T) {
	if _, err := logging.New(logging.Options{Level: "verbose", Out: io.Discard}); err == nil {
		t.Error("an unknown level was accepted. A silent fallback to info leaves an operator " +
			"with logging they cannot discover is wrong")
	}
}

func TestUnknownFormatIsAnError(t *testing.T) {
	if _, err := logging.New(logging.Options{Format: "logfmt", Out: io.Discard}); err == nil {
		t.Error("an unknown format was accepted rather than failing the boot")
	}
}

func TestKnownLevelsAndFormatsAreAccepted(t *testing.T) {
	// The companion to the two tests above: a New that rejected everything would
	// pass both of them.
	for _, lvl := range []string{"", "debug", "info", "warn", "warning", "error", "INFO"} {
		if _, err := logging.New(logging.Options{Level: lvl, Out: io.Discard}); err != nil {
			t.Errorf("level %q rejected: %v", lvl, err)
		}
	}
	for _, f := range []string{"", "json", "text", "JSON"} {
		if _, err := logging.New(logging.Options{Format: f, Out: io.Discard}); err != nil {
			t.Errorf("format %q rejected: %v", f, err)
		}
	}
}

func TestNopWritesNothing(t *testing.T) {
	log := logging.Nop()
	log.Error("this must not appear")
	log.Info("nor this")
	// Nop writes to io.Discard, so the assertion is that it does not panic and
	// that it is usable as a *slog.Logger — the property consumers depend on.
	if log == nil {
		t.Fatal("Nop returned nil, which would nil-dereference at the first call site")
	}
	if log.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Nop is enabled at info level, so a consumer would pay to format lines nobody reads")
	}
}

func TestJobAttrIsUsableFromSlogDirectly(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logging.New(logging.Options{Out: &buf})
	log.Info("x", logging.Job("j", "u", "ocr", map[string]string{"a": "1"}))

	var rec struct {
		Job struct {
			Params []string `json:"params"`
			ID     string   `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec.Job.ID != "j" || len(rec.Job.Params) != 1 || rec.Job.Params[0] != "a" {
		t.Errorf("Job() does not compose with plain slog: %+v", rec.Job)
	}
}

// TestDefaultOutputIsStdoutNotStderr pins the stream.
//
// slog's own default is stderr. Taking it would split the router's output across
// two streams, and a deployment capturing one would silently lose half.
func TestDefaultOutputIsStdoutNotStderr(t *testing.T) {
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	log, newErr := logging.New(logging.Options{}) // Out deliberately unset
	if newErr == nil {
		log.Info("marker-line")
	}
	os.Stdout, os.Stderr = origOut, origErr
	_ = outW.Close()
	_ = errW.Close()

	if newErr != nil {
		t.Fatalf("New: %v", newErr)
	}
	gotOut, _ := io.ReadAll(outR)
	gotErr, _ := io.ReadAll(errR)

	if !strings.Contains(string(gotOut), "marker-line") {
		t.Errorf("nothing reached stdout: %q", gotOut)
	}
	if strings.Contains(string(gotErr), "marker-line") {
		t.Errorf("the line went to stderr, splitting the router's output across two streams: %q",
			gotErr)
	}
}
