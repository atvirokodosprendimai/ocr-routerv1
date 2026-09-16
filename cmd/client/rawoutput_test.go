package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

// pngMagic is deliberately not valid UTF-8 — the byte the whole ADR is about.
const pngMagic = "\x89PNG\r\n\x1a\n"

// TestRawClientWritesBytesVerbatim is the last hop of ADR-0006.
//
// ⚠ It asserts EXACT LENGTH as well as content. Appending a trailing newline is
// the single most likely accidental corruption here, because every other output
// path in this CLI ends with one — and a prefix or contains match would not see
// it.
func TestRawClientWritesBytesVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.bin")
	res := client.Result{JobID: "j1", Raw: []byte(pngMagic)}

	if err := writeResult(&bytes.Buffer{}, path, res, false); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != pngMagic {
		t.Errorf("wrote % x (%d bytes), want % x (%d bytes)",
			got, len(got), pngMagic, len(pngMagic))
	}
}

// TestRawToStdoutIsUncontaminated keeps `-o -` composable, which ADR-0005 made
// the point of putting progress on stderr.
func TestRawToStdoutIsUncontaminated(t *testing.T) {
	var out bytes.Buffer
	res := client.Result{JobID: "j1", Raw: []byte(pngMagic)}

	if err := writeResult(&out, stdoutPath, res, false); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	if out.String() != pngMagic {
		t.Errorf("stdout carried % x, want exactly % x — anything else and `client … -o - | …` "+
			"is not pipeable", out.Bytes(), pngMagic)
	}
}

// TestUnitsClientUnchanged pins ADR-0005's contract: units are still
// newline-joined, with the trailing newline that shape has always had.
func TestUnitsClientUnchanged(t *testing.T) {
	var out bytes.Buffer
	res := client.Result{JobID: "j1", Units: []string{"a", "b"}}

	if err := writeResult(&out, stdoutPath, res, false); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	if out.String() != "a\nb\n" {
		t.Errorf("units output = %q, want \"a\\nb\\n\" — ADR-0005 owns this shape", out.String())
	}
}

// TestRawJSONModeIsRefused: --json renders a units envelope, which cannot carry
// bytes. Refusing is the point — encoding them into it is how the corruption
// this ADR removes would come straight back.
func TestRawJSONModeIsRefused(t *testing.T) {
	var out bytes.Buffer
	res := client.Result{JobID: "j1", Raw: []byte(pngMagic)}

	if err := writeResult(&out, stdoutPath, res, true); err == nil {
		t.Error("--json accepted a raw result — a JSON string field cannot carry arbitrary bytes, " +
			"and encoding/json would replace every invalid one with U+FFFD in silence")
	}
}
