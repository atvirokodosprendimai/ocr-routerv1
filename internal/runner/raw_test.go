package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/runner"
)

// pngMagic is deliberately not valid UTF-8.
//
// 0x89 is a continuation byte with no lead, so encoding/json replaces it with
// U+FFFD — silently, with a nil error, and three bytes where there was one. That
// is the whole reason ADR-0006 exists, and it is why every fixture on this path
// is a real magic number rather than text: "labas" round-trips perfectly through
// the broken channel.
const pngMagic = "\x89PNG\r\n\x1a\n"

// TestRawRunnerReturnsBytesVerbatim asserts the defect at its first hop.
func TestRawRunnerReturnsBytesVerbatim(t *testing.T) {
	// printf, not echo: echo's handling of backslash escapes differs between
	// shells, and this test is about bytes.
	r := newRunner(script(t, `printf '\211PNG\r\n\032\n'`))
	r.Raw = true

	out, err := r.RunRaw(context.Background(), runner.Job{ID: "j1"})
	if err != nil {
		t.Fatalf("RunRaw: %v", err)
	}
	if string(out) != pngMagic {
		t.Errorf("raw stdout = % x (%d bytes), want % x (%d bytes)",
			out, len(out), pngMagic, len(pngMagic))
	}
}

// TestRawRespectsMaxOutput keeps the worker-side memory bound that the units
// contract enforces by accident.
func TestRawRespectsMaxOutput(t *testing.T) {
	r := newRunner(script(t, `head -c 100000 /dev/zero`))
	r.Raw = true
	r.MaxOutput = 1024

	if _, err := r.RunRaw(context.Background(), runner.Job{ID: "j1"}); err == nil {
		t.Error("a raw command far over --max-output succeeded — the cap must bound both arms, " +
			"or a runaway raw service exhausts the worker before its timeout fires")
	}
}

// TestRawArmDoesNotParseUnits pins that the two contracts stay apart: the raw
// arm must not care whether stdout happens to be JSON.
func TestRawArmDoesNotParseUnits(t *testing.T) {
	r := newRunner(script(t, `printf 'not json at all'`))
	r.Raw = true

	out, err := r.RunRaw(context.Background(), runner.Job{ID: "j1"})
	if err != nil {
		t.Fatalf("RunRaw on non-JSON stdout: %v — the raw arm must not apply the units contract", err)
	}
	if string(out) != "not json at all" {
		t.Errorf("raw stdout = %q", out)
	}
}

// TestUnitsArmUnchanged is the other half: the JSON contract still applies when
// the worker is not raw, and its error message still names what was produced.
func TestUnitsArmUnchanged(t *testing.T) {
	r := newRunner(script(t, `printf 'labas'`))

	if _, err := r.Run(context.Background(), runner.Job{ID: "j1"}); err == nil {
		t.Fatal("a units worker accepted non-JSON stdout")
	} else if !strings.Contains(err.Error(), "labas") {
		t.Errorf("the failure does not quote what was produced: %v", err)
	}
}
