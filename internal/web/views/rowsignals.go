package views

import (
	"fmt"
	"strings"
)

// rowSignalRoot is the datastar signal namespace holding per-row edit state.
//
// ⚠ THE LEADING UNDERSCORE IS LOAD-BEARING, not a naming convention. datastar's
// fetch actions build their payload with
// `filterSignals:{include:/.*/, exclude:/(^|\.)_/}` — read from the vendored
// bundle, not from memory — so every path with an underscore-led segment is
// omitted from the request. Without it, one signal per field per customer would
// ship on EVERY action on this page, which is the "no 1000 form fields" failure
// the house datastar rules name explicitly. With it the wire carries only the
// handful of flat signals a handler actually reads, whatever the customer count.
const rowSignalRoot = "_row"

// rowKey names one customer's slice of that namespace.
//
// ⚠ HYPHENS ARE STRIPPED AND A LETTER IS PREFIXED, and neither is cosmetic.
// datastar resolves a signal reference with
// `/\$([a-zA-Z_\d]\w*(?:[.-]\w+)*)/` — `-` and `.` are BOTH path steps to that
// parser, so a raw uuidv7 would read as five nested levels rather than one row
// name. The `u` prefix keeps the segment letter-led for the same reason.
func rowKey(userID string) string {
	return "u" + strings.ReplaceAll(userID, "-", "")
}

// rowBind is the `data-bind` value for one field of one customer's row.
//
// ⚠ THE VALUE FORM, never the `data-bind:key` form. A colon key is case-folded
// (`buffer-limit` → `bufferLimit`), which would rewrite the path segments here;
// the attribute value is taken raw, so what is written is what is bound.
func rowBind(userID, field string) string {
	return fmt.Sprintf("%s.%s.%s", rowSignalRoot, rowKey(userID), field)
}

// rowSubmit builds a row button's click expression: copy that row's own values
// into the flat signals the handler reads, then fire the action.
//
// ⚠ THIS COPY IS WHAT MAKES THE ROWS INDEPENDENT. datastar signals are global
// and flattened page-wide, so binding every row to `creditDelta` gave every
// customer ONE shared box — typing in any row changed them all, and the numbers
// on display belonged to whichever row rendered last. The per-row namespace
// isolates the editing; this hands the chosen row to handlers whose signal
// struct never has to learn that rows exist.
func rowSubmit(userID, action string, fields ...string) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "$%s = $%s; ", f, rowBind(userID, f))
	}
	fmt.Fprintf(&b, "@post('%s')", action)
	return b.String()
}
