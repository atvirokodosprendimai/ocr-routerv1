// Package views_test holds the markup rules.
//
// These tests live BESIDE the views rather than in the sibling handler package,
// because a package with no test file of its own reads as untested to any gate
// that counts them — and "its tests are next door" is exactly the explanation
// nobody is there to give when the gate fires.
package views_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
	"github.com/atvirokodosprendimai/ocr-router/internal/web/views"
)

// render runs a component to a string, the way the handler does.
func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// everyView renders each page and fragment once, so a markup rule can be
// asserted across the whole surface rather than one template at a time.
func everyView(t *testing.T) map[string]string {
	t.Helper()
	d := views.Dashboard{
		Counts: map[core.JobState]int{core.JobQueued: 2, core.JobDone: 1},
		Jobs: []views.JobRow{{
			Job: core.Job{
				ID: "0192f8aa-1111-7000-8000-000000000001", UserID: "u1",
				Label: "ocr", State: core.JobDone, Units: 3, AccruedCredits: 3,
			},
			StageLabel: "—",
		}},
		Users: []core.User{{
			ID: "u1", Email: "c@example.com", Role: core.RoleClient,
			Credits: 10, BufferLimit: 4, Priority: 0, Active: true,
		}},
		Services:        []views.ServiceRow{{Label: "ocr", Workers: 1, Queued: 2, Rate: 1}},
		Emails:          map[string]string{"u1": "c@example.com"},
		ResultsInMemory: 1,
	}
	return map[string]string{
		"Overview":  render(t, views.Overview(d)),
		"Users":     render(t, views.Users(d)),
		"Services":  render(t, views.Services(d)),
		"Stats":     render(t, views.Stats(d)),
		"Jobs":      render(t, views.Jobs(d)),
		"Workers":   render(t, views.Workers(d)),
		"UserTable": render(t, views.UserTable(d)),
	}
}

// TestNoFormTags — the house rule, asserted across every view.
func TestNoFormTags(t *testing.T) {
	for name, html := range everyView(t) {
		if strings.Contains(html, "<form") {
			t.Errorf("%s contains a <form> tag; inputs are bound individually with data-bind:", name)
		}
	}
}

// TestUsesDataInitNotOnLoad guards the single highest-frequency datastar error.
//
// `data-on-load` does not exist in v1. A page using it never subscribes, with no
// console error and nothing in the network tab — it just sits there empty.
func TestUsesDataInitNotOnLoad(t *testing.T) {
	views := everyView(t)
	for name, html := range views {
		if strings.Contains(html, "data-on-load") {
			t.Errorf("%s uses data-on-load, which does not exist in datastar v1 — the page "+
				"would silently never subscribe", name)
		}
	}
	if !strings.Contains(views["Overview"], "data-init=") {
		t.Error("the overview never opens its stream with data-init")
	}
}

// legitimateHyphenated are the six `data-on-*` names that are SEPARATE
// attributes rather than event bindings, and correctly use hyphens.
var legitimateHyphenated = map[string]bool{
	"data-on-intersect": true, "data-on-interval": true,
	"data-on-signal-patch": true, "data-on-signal-patch-filter": true,
	"data-on-raf": true, "data-on-resize": true,
}

// TestDomEventsUseColon catches `data-on-click` and friends.
func TestDomEventsUseColon(t *testing.T) {
	hyphenated := regexp.MustCompile(`data-on-[a-z-]+`)
	for name, html := range everyView(t) {
		for _, found := range hyphenated.FindAllString(html, -1) {
			if !legitimateHyphenated[found] {
				t.Errorf("%s uses %q — DOM events take a COLON (data-on:click). Only the six "+
					"named data-on-* attributes use hyphens", name, found)
			}
		}
	}
}

// TestBindingsAreKebabCase catches the casing trap.
//
// HTML lower-cases attribute names, so `data-bind:newEmail` binds `newemail` —
// a DIFFERENT signal from the `newEmail` the page declared, with nothing
// reporting the mismatch. An earlier session elsewhere lost three debug rounds
// to exactly this.
func TestBindingsAreKebabCase(t *testing.T) {
	suffixed := regexp.MustCompile(`data-(bind|signals|indicator|show|attr|class|text|on):([A-Za-z0-9_.:-]+)`)
	for name, html := range everyView(t) {
		for _, m := range suffixed.FindAllStringSubmatch(html, -1) {
			attr, suffix := m[1], m[2]
			if attr == "on" || attr == "show" || attr == "text" {
				continue // these take expressions or event names, not signal paths
			}
			if strings.ToLower(suffix) != suffix {
				t.Errorf("%s has data-%s:%s — an upper-case letter in an attribute SUFFIX is "+
					"lower-cased by the HTML parser, so this binds a different signal than the "+
					"one declared", name, attr, suffix)
			}
		}
	}
}

// TestStoredTextIsNotInCompiledAttributes is the silent-breakage guard.
//
// `data-signals` is compiled as JavaScript, and the action rewrite runs INSIDE
// quoted strings: text containing `@word(` throws in the expression compiler and
// every later attribute on that element stops running. Stored text therefore
// belongs in a text node, never in a data-* attribute value.
func TestStoredTextIsNotInCompiledAttributes(t *testing.T) {
	hostile := "Call @Anna(invoices)"
	d := views.Dashboard{
		Counts: map[core.JobState]int{},
		Jobs: []views.JobRow{{
			Job: core.Job{
				ID: "0192f8aa-1111-7000-8000-000000000001", UserID: "u1",
				Label: "ocr", State: core.JobDead,
				LastError: hostile,
			},
			StageLabel: "—",
		}},
		Users:  []core.User{{ID: "u1", Email: "a@b(c).com", Role: core.RoleClient, Active: true}},
		Emails: map[string]string{"u1": "a@b(c).com"},
	}

	for name, html := range map[string]string{
		"Jobs":      render(t, views.Jobs(d)),
		"UserTable": render(t, views.UserTable(d)),
	} {
		if !strings.Contains(html, "Anna") && !strings.Contains(html, "a@b(c).com") {
			t.Fatalf("%s did not render the stored text at all — the negative assertion below "+
				"would pass vacuously", name)
		}
		for _, attr := range dataAttributeValues(html) {
			if strings.Contains(attr, "Anna") || strings.Contains(attr, "a@b(c).com") {
				t.Errorf("%s put stored text inside a data-* attribute (%q). data-signals is "+
					"compiled as JavaScript and `@word(` inside it disables every later "+
					"attribute on the element, silently", name, attr)
			}
		}
	}
}

// dataAttributeValues extracts the value of every data-* attribute.
var dataAttrRe = regexp.MustCompile(`data-[a-zA-Z0-9_:.-]+="([^"]*)"`)

func dataAttributeValues(html string) []string {
	var out []string
	for _, m := range dataAttrRe.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestLiveFragmentsHaveStableIDs — an SSE patch morphs by id, and a fragment
// cannot be patched into EXISTENCE: its target must already be in the document.
func TestLiveFragmentsHaveStableIDs(t *testing.T) {
	v := everyView(t)
	for fragment, id := range map[string]string{
		"Stats": `id="stats"`, "Jobs": `id="jobs"`, "Workers": `id="workers"`,
		"UserTable": `id="user-table"`,
	} {
		if !strings.Contains(v[fragment], id) {
			t.Errorf("the %s fragment has no %s — the SSE patch would have nothing to morph",
				fragment, id)
		}
		if !strings.Contains(v["Overview"]+v["Users"], id) {
			t.Errorf("%s is not present in the first paint; a fragment cannot be patched into "+
				"existence, only over a target already in the document", id)
		}
	}
}

func TestEveryInputHasALabel(t *testing.T) {
	inputRe := regexp.MustCompile(`<(input|select)[^>]*id="([^"]+)"`)
	for name, html := range everyView(t) {
		for _, m := range inputRe.FindAllStringSubmatch(html, -1) {
			id := m[2]
			if !strings.Contains(html, `for="`+id+`"`) {
				t.Errorf("%s: <%s id=%q> has no associated <label for=…>", name, m[1], id)
			}
		}
	}
}

func TestEmptyStatesExist(t *testing.T) {
	empty := views.Dashboard{Counts: map[core.JobState]int{}, Emails: map[string]string{}}
	for name, html := range map[string]string{
		"Jobs":      render(t, views.Jobs(empty)),
		"Workers":   render(t, views.Workers(empty)),
		"UserTable": render(t, views.UserTable(empty)),
	} {
		if !strings.Contains(html, "empty") {
			t.Errorf("%s has no empty state — a blank table is indistinguishable from a broken "+
				"page", name)
		}
	}
}

func TestLoadingStateExists(t *testing.T) {
	html := everyView(t)["Users"]
	if !strings.Contains(html, "data-indicator") {
		t.Error("the create action has no data-indicator, so a slow request shows nothing")
	}
	if !strings.Contains(html, "data-show=") {
		t.Error("nothing consumes the indicator signal, so it is set and never displayed")
	}
}

func TestWorkersViewSurfacesQueueWithNoWorker(t *testing.T) {
	// The silent failure this table exists for: a label with work queued and
	// nobody serving it.
	d := views.Dashboard{
		Counts:   map[core.JobState]int{},
		Emails:   map[string]string{},
		Services: []views.ServiceRow{{Label: "ocr", Workers: 0, Queued: 5, Rate: 1}},
	}
	html := render(t, views.Workers(d))
	if !strings.Contains(html, "no worker") {
		t.Error("a label with a queue and zero workers is not called out — that is precisely " +
			"the failure an operator needs to see")
	}
	if !strings.Contains(html, "5") {
		t.Error("the number of waiting jobs is not shown")
	}
}

func TestTokenIsPresentedAsShownOnce(t *testing.T) {
	html := render(t, views.TokenOnce("ocr_c_secrettoken"))
	if !strings.Contains(html, "ocr_c_secrettoken") {
		t.Fatal("the minted token is not rendered")
	}
	if !strings.Contains(strings.ToLower(html), "once") {
		t.Error("the fragment does not tell the operator the token is shown once — they will " +
			"navigate away and lose it")
	}
}
