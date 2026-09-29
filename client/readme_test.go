package client_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ⚠ A README EXAMPLE ROTS SILENTLY, WHICH IS WHY THIS TEST EXISTS.
//
// The same argument as cmd/router/packaging_test.go makes about
// compose.yml.example: rename an exported symbol and nothing anywhere fails.
// The README still renders, the package still builds, every test still passes,
// and the failure arrives as a compile error in somebody else's module after
// they have copied the snippet. Unlike the examples in example_test.go, which
// the compiler checks for free, prose is checked by nobody.
//
// This is deliberately a NAME check and not a compile check: the README snippet
// is a fragment, not a program. What it can catch is the whole realistic
// failure mode — a symbol that was renamed or removed.

// readmeSymbol matches a `client.Ident` reference inside the README.
var readmeSymbol = regexp.MustCompile(`\bclient\.([A-Z][A-Za-z0-9_]*)`)

// exported is the package's public surface, written out rather than reflected.
//
// ⚠ HAND-KEPT ON PURPOSE. Reflecting over the package would make this test
// tautological — it would compare the README against whatever the code happens
// to say today, so a rename would update both sides at once and the check would
// pass through exactly the change it exists to catch. This list is a second,
// independent statement of the contract, and the compiler keeps it honest:
// every name below is referenced in example_test.go, so deleting a symbol
// breaks the build rather than quietly shrinking the list.
var exported = map[string]bool{
	"Submit":          true,
	"Config":          true,
	"Input":           true,
	"Result":          true,
	"Stage":           true,
	"Progress":        true,
	"StageUploading":  true,
	"StageWaiting":    true,
	"StageCollecting": true,
	"StageDone":       true,
	"FailedError":     true,
	"RetryableError":  true,
}

// TestReadmeClientExampleNamesRealSymbols checks that every client.X the README
// names is a symbol this package actually exports.
func TestReadmeClientExampleNamesRealSymbols(t *testing.T) {
	root := repoRoot(t)

	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	matches := readmeSymbol.FindAllStringSubmatch(string(body), -1)

	// ⚠ THE PRECONDITION THAT MAKES THE REST MEAN ANYTHING. Without it this
	// test is a negative assertion — "no bad name appears" — which is also
	// satisfied by a README that mentions the package not at all, by a typo in
	// the path, and by an empty file. Every one of those is a state we want to
	// fail on, and all three look identical to success from the loop below.
	if len(matches) == 0 {
		t.Fatal("README.md names no client.X symbol at all — either the Go package section was removed, or this test stopped looking at the right file")
	}

	var unknown []string
	for _, m := range matches {
		if !exported[m[1]] {
			unknown = append(unknown, m[1])
		}
	}
	sort.Strings(unknown)

	if len(unknown) > 0 {
		t.Errorf("README.md names %d symbol(s) this package does not export: %s\n"+
			"Either the README is stale, or the rename that caused this needs to reach it too.",
			len(unknown), strings.Join(unknown, ", "))
	}
}
