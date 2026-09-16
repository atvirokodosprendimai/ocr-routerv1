// Package blob stores job source files on disk.
//
// A blob is the DURABLE copy of a job's input. Results live only in memory and
// are lost to a restart or a TTL; the blob is what makes that recoverable, so a
// blob outlives its job until the job reaches a terminal state and is never
// deleted earlier.
package blob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// Store is a directory of job source files.
type Store struct {
	dir string
}

// New returns a Store rooted at dir, creating it if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating blob dir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the root directory. The health probe writes a temp file here to
// check the volume is still writable.
func (s *Store) Dir() string { return s.dir }

// validID reports whether id is safe to turn into a path.
//
// The id arrives from a URL path, so this is a trust boundary: a traversal here
// is a read of any file the process can open. It VALIDATES rather than
// sanitises — sanitising invites a second, subtly different sanitiser
// elsewhere, and the two eventually disagree. A uuidv7 string is hex and dashes
// and nothing else, so anything outside that set is simply refused.
func validID(id string) bool {
	// The canonical UUID shape: 8-4-4-4-12, dashes at exactly these offsets.
	//
	// Checking the POSITIONS rather than just "hex or dash somewhere" is what
	// makes this say what it means. A looser check that accepted any 36
	// hex-or-dash characters would pass thirty-six 'a's — still a safe filename,
	// since it contains no separator and no dot, but not an id this system can
	// ever have minted, and a validator that accepts things the system cannot
	// produce is one nobody can reason about later.
	if len(id) != 36 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// path returns the on-disk location for a job id.
//
// The first two characters shard the directory, so one directory never has to
// hold a million entries — which some filesystems handle badly and every `ls`
// handles badly.
func (s *Store) path(id string) (string, error) {
	if !validID(id) {
		return "", core.ErrNotFound
	}
	return filepath.Join(s.dir, id[:2], id), nil
}

// resultPath is where a RAW job's output lives.
//
// The suffix is appended AFTER validID has accepted the id, so it introduces no
// separator and no dot into anything untrusted — the traversal guard still
// governs every byte that came from the URL. A distinct key rather than a
// distinct directory keeps one job's input and output side by side, which
// matters during a pipeline stage: the next stage's input is written while the
// previous stage's output is still readable.
func (s *Store) resultPath(id string) (string, error) {
	full, err := s.path(id)
	if err != nil {
		return "", err
	}
	return full + ".out", nil
}

// PutResult writes r's bytes as the raw output of id.
//
// Same staged write, fsync and atomic rename as Put, and for the same reason: a
// crash must never leave a TRUNCATED result under the real name. A short raw
// payload is indistinguishable from a complete one — there is no syntax to fail
// — so it would be delivered and billed as though it were whole.
func (s *Store) PutResult(id string, r io.Reader) (int64, error) {
	full, err := s.resultPath(id)
	if err != nil {
		return 0, err
	}
	return s.putAt(full, id, r)
}

// OpenResult returns a reader over a raw job's output. The caller closes it.
func (s *Store) OpenResult(id string) (*os.File, error) {
	full, err := s.resultPath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, core.ErrNotFound
	}
	return f, err
}

// DeleteResult removes a raw job's output. Deleting an absent one is not an
// error, for the same reason Delete is idempotent: delivery, the dead letter and
// expiry all reach it, and a job that took two of them must not fail the second.
func (s *Store) DeleteResult(id string) error {
	full, err := s.resultPath(id)
	if err != nil {
		return nil
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Put writes r's bytes as the blob for id, replacing any existing one.
//
// The write is staged in a temp file in the SAME directory, fsync'd, and then
// renamed into place. Rename within a directory is atomic, so a crash partway
// through can leave a stray temp file but can never leave a TRUNCATED blob under
// the real name — and a truncated blob is indistinguishable from a short one,
// which would be OCR'd and billed as though it were the whole document.
func (s *Store) Put(id string, r io.Reader) (int64, error) {
	full, err := s.path(id)
	if err != nil {
		return 0, err
	}
	return s.putAt(full, id, r)
}

// putAt is the staged write both keys share: temp file in the SAME directory,
// fsync, atomic rename. Factored out rather than copied so the source and the
// result blob cannot drift apart on the property that matters — a crash partway
// through leaves a stray temp file, never a truncated blob under the real name.
func (s *Store) putAt(full, id string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-"+id+"-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	// If anything below fails, do not leave the staging file behind.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		return 0, fmt.Errorf("writing blob %s: %w", id, err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("syncing blob %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("closing blob %s: %w", id, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return 0, fmt.Errorf("committing blob %s: %w", id, err)
	}
	return n, nil
}

// PutBytes writes b as the blob for id.
//
// This is how a pipeline stage's output becomes the next stage's input: the
// next stage expects a file at -i, so the bridge between stages is a blob.
func (s *Store) PutBytes(id string, b []byte) (int64, error) {
	return s.Put(id, strings.NewReader(string(b)))
}

// Open returns a reader over the blob. The caller closes it.
func (s *Store) Open(id string) (*os.File, error) {
	full, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, core.ErrNotFound
	}
	return f, err
}

// Size returns the blob's size in bytes.
func (s *Store) Size(id string) (int64, error) {
	full, err := s.path(id)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return 0, core.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Delete removes the blob. Deleting an absent blob is not an error.
//
// Idempotence matters because several paths reach it — delivery, the dead
// letter, expiry, and a pipeline advance replacing the previous stage's input —
// and a job that took two of them must not fail the second time.
func (s *Store) Delete(id string) error {
	full, err := s.path(id)
	if err != nil {
		// An invalid id names no blob, so there is nothing to delete and
		// nothing to report.
		return nil
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
