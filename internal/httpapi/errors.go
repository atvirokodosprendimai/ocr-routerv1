package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// statusFor maps a domain error to an HTTP status.
//
// ONE place, deliberately. Scattering these decisions across handlers is how a
// sentinel added later silently becomes a 500 in three of five call sites, and
// the table-driven test over every sentinel in core is what keeps this honest.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, core.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, core.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, core.ErrNoCredits):
		// 402 is the one status in this table that says something a retry cannot
		// fix: the customer must buy credits.
		return http.StatusPaymentRequired
	case errors.Is(err, core.ErrBufferFull):
		// 429, not 507: this is back-pressure and the client should retry later.
		return http.StatusTooManyRequests
	case errors.Is(err, core.ErrInvalidParam), errors.Is(err, core.ErrInvalidState):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// errorBody is the shape of every failure response.
type errorBody struct {
	Error string `json:"error"`
}

// writeError renders err with its mapped status.
//
// A 500 deliberately does NOT echo the error text: an unmapped error is by
// definition one nobody reasoned about, and its message may carry a file path, a
// SQL fragment or a customer's data. Mapped errors are ours and are safe to say
// out loud.
func writeError(w http.ResponseWriter, err error) {
	status := statusFor(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		msg = "internal error"
	}
	writeJSON(w, status, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
