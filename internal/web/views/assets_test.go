package views_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/web/views"
)

// render is views_test.go's helper, reused here.

// TestStylesheetIsRealCSS is the test whose absence let a completely unstyled
// dashboard ship.
//
// ⚠ The stylesheet was a Go constant interpolated into a `<style>` element, and
// templ treats the contents of `<style>` and `<script>` as RAW TEXT — it does
// not evaluate expressions there. No error, no warning, no build failure: the
// page shipped with the literal characters `{ css }` as its entire stylesheet
// for the life of T10. Every structural assertion about the markup passed the
// whole time, because the markup WAS right; only the bytes inside one element
// were nonsense, and only a human looking at the page could tell.
func TestStylesheetIsRealCSS(t *testing.T) {
	css := string(views.AppCSS())

	if css == "" {
		t.Fatal("the stylesheet is empty")
	}
	// The exact failure that shipped: an uninterpolated templ expression.
	if strings.Contains(css, "{ css }") || strings.Contains(css, "{ loginCSS }") {
		t.Fatalf("the stylesheet contains an uninterpolated template expression:\n%s",
			css[:min(200, len(css))])
	}
	// And it is actual CSS, not merely non-empty: a declaration block with a
	// property and a value.
	rule := regexp.MustCompile(`(?s)[.#:a-zA-Z][^{}]*\{[^{}]*[a-z-]+\s*:\s*[^;}]+[;}]`)
	if !rule.MatchString(css) {
		t.Errorf("the stylesheet contains no parseable rule:\n%s", css[:min(300, len(css))])
	}
	// Rules the dashboard and the login page actually depend on.
	for _, want := range []string{"--bg:", ".card", "table", ".login-card"} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet is missing %q — the login page and the dashboard share one "+
				"sheet, and a missing half is invisible until someone opens that page", want)
		}
	}
}

// TestPagesLinkTheStylesheet — a correct stylesheet nothing references is the
// same unstyled page by another route.
func TestPagesLinkTheStylesheet(t *testing.T) {
	for name, out := range map[string]string{
		"layout": render(t, views.Layout("x", "{}")),
		"login":  render(t, views.Login("", false)),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(out, `rel="stylesheet"`) {
				t.Fatalf("the page links no stylesheet:\n%s", out[:min(400, len(out))])
			}
			if !strings.Contains(out, "/admin/assets/app.css") {
				t.Error("the page does not reference the served stylesheet")
			}
			// ⚠ The inline form is what broke. If it comes back, so does the bug.
			if strings.Contains(out, "{ css }") || strings.Contains(out, "{ loginCSS }") {
				t.Error("the page carries an uninterpolated template expression — the exact " +
					"failure that shipped an unstyled dashboard")
			}
		})
	}
}

// TestStylesheetHrefIsCacheBusted — the asset is served `immutable` for a year,
// which is only safe because a changed file is a changed URL.
func TestStylesheetHrefIsCacheBusted(t *testing.T) {
	out := render(t, views.Layout("x", "{}"))
	if !strings.Contains(out, "app.css?v=") {
		t.Error("the stylesheet href carries no digest, so a year-long immutable cache would " +
			"pin every browser to the stylesheet it first saw")
	}
	if !strings.Contains(out, views.AppCSSDigest()) {
		t.Errorf("the href digest does not match the served bytes (want %s)", views.AppCSSDigest())
	}
}

// TestNoExternalHostsInRenderedPages is the supply-chain assertion.
//
// ⚠ The dashboard used to load datastar from `cdn.jsdelivr.net`. That made the
// console dead in any deployment without public egress, leaked its usage to a
// third party, and put a remote host's script inside the page that MINTS API
// TOKENS — where a hijacked CDN path is arbitrary code execution on the highest
// value page in the system.
func TestNoExternalHostsInRenderedPages(t *testing.T) {
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//[^"]+`)

	for name, out := range map[string]string{
		"layout": render(t, views.Layout("x", "{}")),
		"login":  render(t, views.Login("", false)),
		"tls":    render(t, views.Login("", true)),
	} {
		t.Run(name, func(t *testing.T) {
			if m := external.FindAllString(out, -1); len(m) > 0 {
				t.Errorf("the page loads resources from outside this binary: %v", m)
			}
			// The assertion is not vacuous: the page really does reference assets.
			if !strings.Contains(out, "/admin/assets/") {
				t.Error("the page references no local assets at all, so the check above proved " +
					"nothing")
			}
		})
	}
}

// TestDatastarBundleMatchesItsRecordedDigest is what makes vendoring a remote
// script honest.
//
// Copying a third-party file into a repository without recording what it was
// makes it unverifiable — the next reader cannot distinguish a legitimate
// upgrade from a substitution. This fails on any change to the bytes that does
// not also update the constant deliberately.
func TestDatastarBundleMatchesItsRecordedDigest(t *testing.T) {
	if got := views.DatastarDigest(); got != views.DatastarSHA256 {
		t.Errorf("the vendored datastar bundle has digest %s but the record says %s.\n"+
			"If this was a deliberate upgrade, update DatastarSHA256 and DatastarVersion in the "+
			"same commit and say where the bytes came from. If it was not, something replaced a "+
			"script that runs on the page that mints API tokens.", got, views.DatastarSHA256)
	}
}

func TestDatastarBundleIsNotEmpty(t *testing.T) {
	js := views.DatastarJS()
	if len(js) < 1024 {
		t.Fatalf("the datastar bundle is %d bytes — an empty or truncated embed would leave "+
			"every interactive control on the dashboard inert, with no error anywhere", len(js))
	}
	if !strings.Contains(string(js[:min(200, len(js))]), "Datastar") {
		t.Error("the embedded bundle does not look like datastar")
	}
}

// TestEditableRowUsesKebabCaseBindings catches a failure with no symptom.
//
// ⚠ HTML LOWER-CASES ATTRIBUTE NAMES. `data-bind:bufferLimit` binds the signal
// `bufferlimit`, which is a different signal from `bufferLimit` — so the field
// never saves, and nothing anywhere reports it: no console error, no failed
// request, no server-side clue. Only a check over the rendered markup finds it.
func TestEditableRowUsesKebabCaseBindings(t *testing.T) {
	out := render(t, views.UserTable(views.Dashboard{
		Users: []core.User{{
			ID: "u1", Email: "c@example.com", Role: core.RoleClient,
			Credits: 100, BufferLimit: 4, Active: true,
		}},
	}))

	for _, want := range []string{
		"data-bind:buffer-limit",
		"data-bind:priority",
		"data-bind:job-ttl",
		"data-bind:credit-delta",
		"data-bind:credit-reason",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the editable row is missing %q", want)
		}
	}
	// The camel spellings must appear nowhere: each one silently binds a
	// different, never-read signal.
	for _, bad := range []string{
		"data-bind:bufferLimit", "data-bind:jobTtl",
		"data-bind:creditDelta", "data-bind:creditReason",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("%q binds a lower-cased signal name that nothing reads, so the field "+
				"silently never saves", bad)
		}
	}
}

// TestCreditsControlSaysAdjustNotSet — a box labelled Credits beside a balance
// of 500 reads as the current value, and an operator typing 500 meaning "make it
// 500" would add 500.
func TestCreditsControlSaysAdjustNotSet(t *testing.T) {
	out := render(t, views.UserTable(views.Dashboard{
		Users: []core.User{{
			ID: "u1", Email: "c@example.com", Role: core.RoleClient,
			Credits: 500, BufferLimit: 4, Active: true,
		}},
	}))

	if !strings.Contains(out, "Adjust credits") {
		t.Error("the credit control is not labelled as an adjustment")
	}
	if !strings.Contains(out, "+50 or -20") {
		t.Error("the credit input carries no sign hint, so it reads as an absolute value")
	}
	if !strings.Contains(out, "reason (required)") {
		t.Error("the reason field does not say it is required")
	}
	// The balance is a ledger; nothing in this system sets it.
	if strings.Contains(out, "Set credits") || strings.Contains(out, "Set balance") {
		t.Error("a set-credits control exists — every movement must be an adjustment with a " +
			"ledger entry")
	}
}

// TestEditableRowHasAccessibleLabels — the inputs have no visible <label>,
// because a table cell has no room for one. Without aria-label they are five
// unlabelled number boxes to a screen reader.
func TestEditableRowHasAccessibleLabels(t *testing.T) {
	out := render(t, views.UserTable(views.Dashboard{
		Users: []core.User{{
			ID: "u1", Email: "c@example.com", Role: core.RoleClient,
			Credits: 100, BufferLimit: 4, Active: true,
		}},
	}))

	for _, want := range []string{
		"Buffer limit for c@example.com",
		"Priority for c@example.com",
		"Job TTL in seconds for c@example.com",
		"Credit adjustment for c@example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no accessible label %q — the inputs sit in table cells with no room for a "+
				"visible <label>, so aria-label is the only thing naming them", want)
		}
	}
}

// TestDeactivateButtonSaysWhatItDoes — the consequence exceeds the affordance:
// one click in a table row stops every token the customer holds.
func TestDeactivateButtonSaysWhatItDoes(t *testing.T) {
	out := render(t, views.UserTable(views.Dashboard{
		Users: []core.User{{
			ID: "u1", Email: "c@example.com", Role: core.RoleClient,
			Credits: 100, BufferLimit: 4, Active: true,
		}},
	}))

	if !strings.Contains(out, "Stops every token") {
		t.Error("the deactivate control does not say what it does. ADR-0001 called it the blunt " +
			"instrument, and a control whose consequence is invisible is one that gets clicked " +
			"by mistake")
	}
}
