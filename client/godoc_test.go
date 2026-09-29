package client_test

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠ AN EXAMPLE THAT COMPILES IS NOT AN EXAMPLE THAT IS DOCUMENTED, AND THE
// DIFFERENCE IS A NAMING CONVENTION NOTHING ELSE CHECKS.
//
// godoc attaches an example to a symbol by its function NAME: Example, or
// Example<Symbol>, or Example<Symbol>_<suffix>. Rename ExampleSubmit_progress
// to ExampleProgressShape and it still compiles, still passes, and silently
// stops appearing beside Submit for every reader. That is rung 3 — "the caller
// can discover it" — and a test-only check cannot see it.
//
// ⚠ THIS REPLACES A CHECK THAT COULD NEVER HAVE PASSED. ADR-0011-T2 originally
// fenced this as `go doc ./client Submit | grep -q Example`. The `go doc` CLI
// does not render examples AT ALL — example rendering is a godoc/pkgsite
// feature — so that command emits no "Example" however correct the code is.
// Found by an independent review on 2026-09-29, reproduced on go1.26.6. The
// lesson is the general one: a gate asserting that a TOOL reports something
// must be run once against a known-good tree, or it encodes the author's
// belief about the tool rather than a fact about the code.
//
// go/doc is the package godoc itself uses, so this asks the question at the
// same layer that answers it for a reader.

// TestExamplesAreAttachedForGodoc parses this package the way godoc does and
// checks the examples actually bind to the symbols they are written about.
func TestExamplesAreAttachedForGodoc(t *testing.T) {
	root := repoRoot(t)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(root, "client"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the client package: %v", err)
	}

	// The examples live in the external test package, which is where godoc
	// looks for them too.
	testPkg, ok := pkgs["client_test"]
	if !ok {
		t.Fatal("no client_test package found — the examples must live in the external test package for godoc to render them")
	}

	files := make([]*ast.File, 0, len(testPkg.Files))
	for _, f := range testPkg.Files {
		files = append(files, f)
	}

	examples := doc.Examples(files...)
	if len(examples) == 0 {
		t.Fatal("go/doc found no examples at all — either example_test.go was removed, or every function in it stopped matching the Example… naming convention that makes it documentation rather than a test")
	}

	// A package-level Example (named "") is what a reader meets first on the
	// package page, and at least one example must attach to Submit, which is
	// the only entry point this package has.
	var hasPackageExample, hasSubmitExample bool
	var names []string
	for _, ex := range examples {
		names = append(names, "Example"+ex.Name)
		switch {
		case ex.Name == "":
			hasPackageExample = true
		case ex.Name == "Submit" || strings.HasPrefix(ex.Name, "Submit_"):
			hasSubmitExample = true
		}
	}

	if !hasPackageExample {
		t.Errorf("no package-level Example() — that is the one a reader sees at the top of the package page.\nfound: %s", strings.Join(names, ", "))
	}
	if !hasSubmitExample {
		t.Errorf("no example attaches to Submit — it must be named Example Submit or ExampleSubmit_<suffix> to render beside the function.\nfound: %s", strings.Join(names, ", "))
	}
}
