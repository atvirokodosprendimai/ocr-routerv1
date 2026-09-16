package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/atvirokodosprendimai/ocr-router/internal/client"
)

// stdoutPath is the conventional "write to stdout" destination.
const stdoutPath = "-"

// probeDestination proves the output path is writable BEFORE anything is
// uploaded.
//
// ⚠ THE ORDER MATTERS FOR MONEY. `GET /files/{id}` deletes the in-memory result
// and CHARGES THE CREDITS, so discovering an unwritable `-o` afterwards means
// the customer paid for a result that goes nowhere and cannot be fetched again.
// Failing here costs nothing.
func probeDestination(path string) error {
	if path == stdoutPath {
		return nil
	}

	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("%s is a directory", path)
	case err == nil && !info.Mode().IsRegular():
		// ⚠ A DEVICE, FIFO OR SOCKET — `-o /dev/null` is the common case, and it
		// is a legitimate way to say "I only want the exit code". Opening it is
		// the whole probe: there is no temp-and-rename to prepare, and the
		// rename would be actively destructive, replacing the device node with a
		// regular file.
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("cannot write to %s: %w", path, err)
		}
		return f.Close()
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ocrr-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// writeResult renders and writes the units.
//
// ⚠ ATOMIC, via a temp file in the DESTINATION'S directory and a rename. The
// result has already been collected and charged by the time this runs, so a
// partial write on a full disk would lose work the customer paid for and cannot
// re-fetch. A temp file elsewhere would make the rename a cross-device copy,
// which is not atomic and defeats the point.
// ⚠ `stdout` is a parameter rather than `os.Stdout`, so a test can capture what
// `-o -` produces. Writing to the global directly made the stdout/stderr split
// untestable, and that split is the whole reason `-o -` is safe to pipe.
func writeResult(stdout io.Writer, path string, res client.Result, asJSON bool) error {
	body, err := render(res, asJSON)
	if err != nil {
		return err
	}

	if path == stdoutPath {
		// Nothing to be atomic about: a pipe has no partial-file state, and the
		// caller sees exactly what arrives.
		_, err := stdout.Write(body)
		return err
	}
	// ⚠ A non-regular destination is written DIRECTLY. `-o /dev/null` is the
	// common case, and renaming a temp file over a device node would replace it
	// with a regular file — destroying something the system needs, to protect
	// against a partial write that cannot happen there anyway.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(body)
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ocrr-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// A failure anywhere below must not leave a stray temp file beside the
		// destination.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	// fsync before rename: a rename that lands before the data is durable gives
	// a crash an intact name over empty content.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// render turns a result into the bytes that go to the destination.
//
// ⚠ A RAW RESULT IS RETURNED UNCHANGED — no trailing newline, no join, no
// encoding. Appending a newline is the single most likely accidental corruption
// here, because every other output path in this CLI ends with one, which is why
// the test asserts exact LENGTH rather than a prefix.
func render(res client.Result, asJSON bool) ([]byte, error) {
	if len(res.Raw) > 0 {
		if asJSON {
			// Refused rather than encoded. The --json envelope is a units shape,
			// and putting bytes through a JSON string field is exactly the
			// silent U+FFFD corruption ADR-0006 exists to remove — it would come
			// straight back at the last hop.
			return nil, fmt.Errorf("--json cannot represent a raw result: a JSON string field " +
				"cannot carry arbitrary bytes. Write it to a file with -o, or to stdout with -o -")
		}
		return res.Raw, nil
	}

	if asJSON {
		b, err := json.MarshalIndent(map[string]any{
			"job_id": res.JobID,
			"units":  res.Units,
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil
	}

	// Newline-joined text: the natural form, and the same shape ADR-0001
	// specified for a worker's own stdout. This is a UNITS contract (ADR-0005)
	// and stays one.
	if len(res.Units) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(res.Units, "\n") + "\n"), nil
}
