package httpapi

import (
	"fmt"
	"net/http"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// rawParam reads a caller's declared output mode from `?raw=`.
//
// ABSENT MEANS UNITS, and that is the load-bearing half (ADR-0006). An
// un-upgraded worker or client sends nothing, and it must read as units rather
// than as "whatever the service is configured for" — inferring the mode from the
// admin record would make an old caller silently correct on a raw service, which
// is precisely the agreement the three-party check exists to enforce.
//
// Anything other than the four accepted spellings is REFUSED rather than
// defaulted. A typo would otherwise mean units, and a caller who meant raw would
// get a mismatch refusal naming a mode it never asked for.
func rawParam(r *http.Request) (bool, error) {
	switch v := r.URL.Query().Get("raw"); v {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("%w: raw must be 0 or 1, got %q", core.ErrInvalidParam, v)
	}
}
