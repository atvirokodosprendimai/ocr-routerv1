package client_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ⚠ THE DECISION THIS PACKAGE EXISTS TO HOLD IS ITS LOCATION, AND NOTHING ELSE
// IN THE BUILD CAN SEE IT.
//
// Go's `internal/` rule is a compiler-enforced import boundary: a package under
// it is importable only from inside this module. So moving this package back
// under `internal/` would unpublish it for every external consumer while the
// tree still compiles, every other test still passes, and `go vet` stays
// silent. The regression is invisible to the entire rest of the gate, and the
// only party who finds out is someone outside this repository whose build
// breaks. That is why ADR-0011 names this file as its `Enforced-by:`.

// repoRoot walks up from the test's working directory to the directory holding
// go.mod. The walk is used rather than a fixed "../" because the answer must
// not change if this file is ever moved deeper.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found walking up from the test's working directory")
		}
		dir = parent
	}
}

// TestClientPackageIsImportable is ADR-0011's Enforced-by check: the protocol
// lives at the module root and not under internal/.
func TestClientPackageIsImportable(t *testing.T) {
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "client", "client.go")); err != nil {
		t.Errorf("client/client.go must exist at the module root so other modules can import it: %v", err)
	}

	// The negative half needs the positive half above to mean anything: on its
	// own, "internal/client is absent" is also satisfied by a checkout where
	// nothing exists at all.
	if _, err := os.Stat(filepath.Join(root, "internal", "client")); err == nil {
		t.Error("internal/client has reappeared — a package under internal/ is importable only from inside this module, so this silently unpublishes the client for every external consumer (ADR-0011)")
	}
}

// TestCorpusPointersDoNotNameInternalClient guards the half of the move that
// nothing else would catch.
//
// ⚠ A DIRECTORY MOVE STRANDS MACHINE-READ POINTERS IN OTHER RECORDS, SILENTLY.
// ADR-0005 and ADR-0006 both carry `Governs:` paths for this package, and
// ADR-0005's `Enforced-by:` names a test file inside it. Those headers are
// resolved against the tree by `adr-context` and `adr-state`. When they stop
// resolving, nothing fails: the build is fine, the tests are fine, and the
// tooling simply begins answering "none governs" for this code. Measured on
// another corpus 2026-08-28: seven records lost their paths to one move and
// every gate stayed green for two days.
func TestCorpusPointersDoNotNameInternalClient(t *testing.T) {
	root := repoRoot(t)

	records := []string{
		filepath.Join("docs", "adr", "0005", "0005-cmd-client.md"),
		filepath.Join("docs", "adr", "0006", "0006-raw-passthrough.md"),
	}

	for _, rel := range records {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)
			continue
		}

		// Only the machine-read headers are checked. The prose bodies and task
		// files legitimately say `internal/client`: they are historical
		// accounts of work done when that path existed, and rewriting them
		// would falsify where the work happened (ADR-0011 §Out of Scope).
		var checked int
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "**Governs:**") && !strings.HasPrefix(line, "**Enforced-by:**") {
				continue
			}
			checked++
			if strings.Contains(line, "internal/client") {
				t.Errorf("%s: header still names internal/client, which no longer resolves:\n  %s", rel, line)
			}
		}
		if checked == 0 {
			t.Errorf("%s: found no Governs:/Enforced-by: header to check — this test cannot pass by matching nothing", rel)
		}
	}
}

// enforcedBy matches an `Enforced-by:` header naming `path::TestName`.
var enforcedBy = regexp.MustCompile("`([^`]+\\.go)::([A-Za-z0-9_]+)`")

// TestEnforcedByPointersResolve checks that the repaired pointers point at
// something real.
//
// ⚠ ABSENCE OF THE OLD STRING IS NOT PRESENCE OF A WORKING POINTER, and the
// test above only proves the former. `client/client.go::TestStreamOpensBeforeUpload`,
// a path with a typo, or a test name that was since renamed all satisfy it.
// Since ADR-0011's whole stated risk is "pointers that stop resolving", the
// weaker check is not the one the record needs. Raised by an independent review
// on 2026-09-29.
//
// This resolves both halves: the file must exist, and it must declare a
// function by that name.
func TestEnforcedByPointersResolve(t *testing.T) {
	root := repoRoot(t)

	records := []string{
		filepath.Join("docs", "adr", "0005", "0005-cmd-client.md"),
		filepath.Join("docs", "adr", "0006", "0006-raw-passthrough.md"),
		filepath.Join("docs", "adr", "0011", "0011-public-client-package.md"),
	}

	for _, rel := range records {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)
			continue
		}

		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "**Enforced-by:**") {
				continue
			}
			// `None — <reason>` is a first-class answer: most decisions have no
			// cheap mechanical enforcement, and a record that says so carries
			// more information than one naming a check that cannot fail.
			m := enforcedBy.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			path, testName := m[1], m[2]

			src, err := os.ReadFile(filepath.Join(root, path))
			if err != nil {
				t.Errorf("%s: Enforced-by names %s, which does not exist: %v", rel, path, err)
				continue
			}
			if !strings.Contains(string(src), "func "+testName+"(") {
				t.Errorf("%s: Enforced-by names %s::%s, but that file declares no such function — the pointer resolves to a file and not to a check", rel, path, testName)
			}
		}
	}
}
