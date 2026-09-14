package blob_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/blob"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

func newStore(t *testing.T) (*blob.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := blob.New(dir)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}
	return s, dir
}

func TestBlobPutOpenRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	id := core.NewID()
	want := "the quick brown fox\x00\xff binary too"

	n, err := s.Put(id, strings.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(want)) {
		t.Errorf("Put wrote %d bytes, want %d", n, len(want))
	}

	f, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != want {
		t.Errorf("round-trip = %q, want %q", got, want)
	}

	size, err := s.Size(id)
	if err != nil || size != int64(len(want)) {
		t.Errorf("Size = %d, %v; want %d", size, err, len(want))
	}
}

func TestBlobPutIsAtomic(t *testing.T) {
	s, dir := newStore(t)
	id := core.NewID()
	if _, err := s.Put(id, strings.NewReader("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A replacing write must not leave a staging file behind, and must leave the
	// final name holding the complete new content rather than a mixture.
	if _, err := s.Put(id, strings.NewReader("second-and-longer")); err != nil {
		t.Fatalf("Put replace: %v", err)
	}

	f, _ := s.Open(id)
	defer f.Close()
	got, _ := os.ReadFile(f.Name())
	if string(got) != "second-and-longer" {
		t.Errorf("after replace = %q, want %q", got, "second-and-longer")
	}

	var strays []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(filepath.Base(p), ".tmp-") {
			strays = append(strays, p)
		}
		return nil
	})
	if len(strays) != 0 {
		t.Errorf("staging files left behind: %v — a crash may leave one, but a successful "+
			"write must not", strays)
	}
}

func TestBlobPutCleansUpStagingOnReadError(t *testing.T) {
	s, dir := newStore(t)
	id := core.NewID()
	if _, err := s.Put(id, failingReader{}); err == nil {
		t.Fatal("Put with a failing reader returned nil error")
	}
	// The blob must not exist, and no staging file may be left.
	if _, err := s.Open(id); err != core.ErrNotFound {
		t.Errorf("Open after a failed Put = %v, want core.ErrNotFound — a failed write must "+
			"not leave a partial blob under the real name", err)
	}
	var files []string
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Errorf("files left after a failed Put: %v", files)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func TestBlobRejectsTraversalID(t *testing.T) {
	s, dir := newStore(t)

	// A canary outside the store, to prove nothing escaped.
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified"), 0o600); err != nil {
		t.Fatalf("writing canary: %v", err)
	}

	bad := []string{
		"../../etc/passwd",
		"..",
		".",
		"a/b",
		"",
		"../secret.txt",
		strings.Repeat("a", 36),                // right length, wrong alphabet
		"0192f8aa-XXXX-7000-8000-000000000000", // right shape, invalid hex
		"0192f8aa-1234-7000-8000-00000000000",  // one char short
		"/etc/passwd",
	}
	for _, id := range bad {
		if _, err := s.Open(id); err != core.ErrNotFound {
			t.Errorf("Open(%q) = %v, want core.ErrNotFound — an id reaches this from a URL "+
				"path, so anything but a refusal is a read of an arbitrary file", id, err)
		}
		if _, err := s.Put(id, strings.NewReader("x")); err != core.ErrNotFound {
			t.Errorf("Put(%q) = %v, want core.ErrNotFound", id, err)
		}
	}

	// The canary is untouched.
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "classified" {
		t.Errorf("the file outside the store was disturbed: %q, %v", got, err)
	}
}

func TestBlobAcceptsRealUUIDv7(t *testing.T) {
	// The complement of the traversal test: the validator must not be so strict
	// that it rejects the ids the system actually mints. A guard that refuses
	// everything passes every negative test and breaks the product.
	s, _ := newStore(t)
	for i := 0; i < 50; i++ {
		id := core.NewID()
		if _, err := s.Put(id, strings.NewReader("ok")); err != nil {
			t.Fatalf("Put(%q) with a freshly minted uuidv7 = %v, want success", id, err)
		}
	}
}

func TestBlobDeleteIsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	id := core.NewID()
	if _, err := s.Put(id, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Several paths reach Delete — delivery, dead-letter, expiry, a pipeline
	// advance replacing the previous stage's input — so a job that takes two of
	// them must not fail on the second.
	if err := s.Delete(id); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(id); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
	if err := s.Delete(core.NewID()); err != nil {
		t.Errorf("Delete of an absent blob: %v, want nil", err)
	}
	if err := s.Delete("not-an-id"); err != nil {
		t.Errorf("Delete of an invalid id: %v, want nil", err)
	}
	if _, err := s.Open(id); err != core.ErrNotFound {
		t.Errorf("Open after Delete = %v, want core.ErrNotFound", err)
	}
}

func TestBlobPutBytesBridgesPipelineStages(t *testing.T) {
	s, _ := newStore(t)
	id := core.NewID()
	// This is how a stage's output becomes the next stage's input.
	if _, err := s.PutBytes(id, []byte("<html>page</html>")); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	f, err := s.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, _ := os.ReadFile(f.Name())
	if string(got) != "<html>page</html>" {
		t.Errorf("PutBytes round-trip = %q", got)
	}
}

func TestBlobSizeOfMissing(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Size(core.NewID()); err != core.ErrNotFound {
		t.Errorf("Size of a missing blob = %v, want core.ErrNotFound", err)
	}
}

func TestBlobDirIsReported(t *testing.T) {
	s, dir := newStore(t)
	if s.Dir() != dir {
		t.Errorf("Dir() = %q, want %q — the health probe needs it to check writability", s.Dir(), dir)
	}
}
