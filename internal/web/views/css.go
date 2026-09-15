package views

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// appCSS is the whole stylesheet, served as a real file.
//
// ⚠ IT USED TO BE A Go CONSTANT INTERPOLATED INTO A <style> ELEMENT, and that
// did not work — it shipped for the life of T10 with the literal text `{ css }`
// as the page's entire stylesheet, so the dashboard rendered completely
// unstyled. templ treats the contents of <style> and <script> as RAW TEXT and
// does not evaluate expressions inside them: no error, no warning, no build
// failure, just a brace-wrapped identifier sitting in the document. The comment
// that used to live here asserted the opposite, which is why nobody looked.
//
// The lesson is narrower than "test your CSS": a templating language that
// silently declines to interpolate in one context will hand you a page that
// compiles, renders, passes every structural assertion, and is visibly broken
// only to a human looking at it. TestStylesheetIsRealCSS asserts the served
// bytes contain actual rules.
//
// Deliberately small and unfashionable: system fonts, one accent, a single
// breakpoint. An operator console is read at 3am by someone who is annoyed, and
// the job is legibility rather than personality.
//
//go:embed assets/app.css
var appCSS []byte

// AppCSS returns the stylesheet bytes.
func AppCSS() []byte { return appCSS }

// AppCSSDigest is used to build the cache-busting query on the <link> href, so a
// deployed browser picks up a changed stylesheet without a hard refresh.
func AppCSSDigest() string {
	sum := sha256.Sum256(appCSS)
	return hex.EncodeToString(sum[:])[:12]
}
