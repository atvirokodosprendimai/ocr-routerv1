package agent_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheInputFileKeepsItsExtension is the defect this fix exists for, observed
// where it actually bit: in the path handed to `--cmd`.
//
// The materialised input was named after the bare job id with no extension, so
// every tool that detects format by extension was handed a file it could not
// identify. Reported 2026-09-28 from a real worker running LibreOffice:
// `libreoffice --headless --cat <uuid>` answers "source file could not be
// loaded" and the job fails for a reason nothing in the router explains.
//
// ⚠ IT ASSERTS THE PATH THE COMMAND RECEIVES, not the naming function. The unit
// table beside this covers the sanitising; this covers the WIRING — that the
// filename the router sends on claim reaches the file on disk. A correct
// inputExt that nothing called would pass that table and fail this.
func TestTheInputFileKeepsItsExtension(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-1", "the source", "quarterly report.docx")

	// Echo the path of the file the command was given, so the test sees exactly
	// what LibreOffice would have seen.
	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$in_file"
`)
	_, tmp, _ := startAgent(t, f, svc, 1)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	got := f.reportsSnapshot()[0]
	if got.Error != "" {
		t.Fatalf("the job failed: %q", got.Error)
	}
	path := got.Units[0]

	if filepath.Ext(path) != ".docx" {
		t.Errorf("the command was handed %q, which has extension %q — LibreOffice and every "+
			"other format-sniffing tool cannot identify it", path, filepath.Ext(path))
	}
	// ⚠ The BASENAME must still be the job id, not the client's filename. The
	// client chose "quarterly report.docx"; a space in a path is survivable and
	// the rest of what a client can put in a filename is not.
	if base := filepath.Base(path); base != "job-1.docx" {
		t.Errorf("the input is named %q, want job-1.docx — the client's basename reached the "+
			"filesystem", base)
	}
	if dir := filepath.Dir(path); dir != strings.TrimSuffix(tmp, "/") {
		t.Errorf("the input landed in %q, outside the configured tmpdir %q", dir, tmp)
	}
}

// TestAClientFilenameCannotPlaceTheInputFile is the same wiring, driven by a
// filename chosen to escape.
//
// It is the test that would have caught the obvious implementation of this fix —
// using the filename the router sends — which works perfectly for
// "report.docx" and writes wherever it likes for this one.
func TestAClientFilenameCannotPlaceTheInputFile(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-2", "the source", "../../../../tmp/pwned.pdf")

	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$in_file"
`)
	_, tmp, _ := startAgent(t, f, svc, 1)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	path := f.reportsSnapshot()[0].Units[0]
	if base := filepath.Base(path); base != "job-2.pdf" {
		t.Errorf("the input is named %q, want job-2.pdf", base)
	}
	if dir := filepath.Dir(path); dir != strings.TrimSuffix(tmp, "/") {
		t.Errorf("a client filename placed the input at %q, outside the tmpdir %q — any customer "+
			"can name an upload anything, and this path is on a host they do not own", dir, tmp)
	}
	// The extension is still useful: a traversal attempt does not cost the
	// worker its format hint.
	if filepath.Ext(path) != ".pdf" {
		t.Errorf("extension = %q, want .pdf", filepath.Ext(path))
	}
}

// TestAJobWithNoFilenameStillRuns keeps the old behaviour where there is nothing
// to take — the crawler shape sends no blob and nothing named it.
func TestAJobWithNoFilenameStillRuns(t *testing.T) {
	f := newFakeRouter()
	f.enqueueNamed("job-3", "the source", "")

	svc := script(t, `
while [ $# -gt 0 ]; do case "$1" in -i) in_file="$2"; shift 2 ;; *) shift ;; esac; done
printf '["%s"]' "$in_file"
`)
	_, _, _ = startAgent(t, f, svc, 1)
	f.events <- "work"

	waitFor(t, func() bool { return len(f.reportsSnapshot()) == 1 }, 5*time.Second,
		"the agent never reported a result")

	got := f.reportsSnapshot()[0]
	if got.Error != "" {
		t.Fatalf("a job with no filename failed: %q", got.Error)
	}
	if base := filepath.Base(got.Units[0]); base != "job-3" {
		t.Errorf("the input is named %q, want the bare job id", base)
	}
}
