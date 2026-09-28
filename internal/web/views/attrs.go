package views

import "github.com/a-h/templ"

// currentPage renders `aria-current="page"` when this is the page or filter the
// operator is looking at, and NOTHING when it is not.
//
// ⚠ IT EXISTS BECAUSE `templ.KV` DOES NOT DO THIS, and the wrong spelling fails
// silently in the worst way: `aria-current={ templ.KV("page", cond) }` compiles,
// renders, and puts the Go value's %v form into the document —
// `aria-current="{page true}"`. Measured against the running binary on
// 2026-09-28: the job-filter nav had been serving exactly that since ADR-0007
// shipped it, so BOTH links carried an aria-current, the CSS rule
// `nav a[aria-current="page"]` matched neither, and a screen reader was told
// something no specification defines.
//
// `templ.KV` is for the boolean-attribute form — `disabled?={ templ.KV(…) }` —
// where the attribute has no value to carry. A conditional attribute WITH a value
// needs spread attributes, which is this, or an if/else around the whole element.
//
// Returning nil rather than an empty map is deliberate: templ spreads nothing,
// so the attribute is absent rather than present-and-empty. `aria-current=""` is
// not the same as no aria-current — the first is a value the spec does not define.
func currentPage(isCurrent bool) templ.Attributes {
	if !isCurrent {
		return nil
	}
	return templ.Attributes{"aria-current": "page"}
}
