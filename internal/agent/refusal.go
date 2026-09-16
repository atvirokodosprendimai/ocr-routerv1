package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// fatalRefusal is a router answer that RETRYING CANNOT CHANGE.
//
// It exists because the agent's reconnect loop is built for the transient case —
// a dropped stream, a restarting router — and applying it to a configuration
// error produces the worst failure mode this worker has: an endless quiet loop
// whose log says "409 Conflict" and nothing else, while the fix is one line in a
// script or one checkbox on a page.
//
// ADR-0006's risk table promised this behaviour and the first implementation did
// not deliver it: a worker started `--raw` against a service the administrator
// had not marked raw reconnected every second, forever.
type fatalRefusal struct {
	Status string
	Detail string
}

func (e *fatalRefusal) Error() string {
	if e.Detail == "" {
		return e.Status
	}
	return e.Status + ": " + e.Detail
}

// classifyStreamStatus decides whether a non-200 subscribe is worth retrying.
//
// ⚠ THE SPLIT IS ABOUT WHAT A RETRY COULD ACHIEVE, not about severity. Every 4xx
// here is the router describing this worker's CONFIGURATION — an unknown label,
// a revoked token, the wrong role, a mode the operator did not configure — and
// every one of them answers identically on the next attempt.
//
// 429 is the exception and the reason this is not simply "4xx is fatal": rate
// limiting is explicitly temporary, and backing off is the correct response to
// it. 5xx is a router that may come back, which is what the loop is for.
func classifyStreamStatus(code int, status string, body []byte) error {
	detail := refusalDetail(body)
	if code >= 400 && code < 500 && code != http.StatusTooManyRequests {
		return &fatalRefusal{Status: "stream returned " + status, Detail: detail}
	}
	if detail == "" {
		return fmt.Errorf("stream returned %s", status)
	}
	return fmt.Errorf("stream returned %s: %s", status, detail)
}

// refusalDetail pulls the router's own explanation out of its error envelope.
//
// Best effort by design: a proxy or a crash can answer with something that is
// not the router's JSON, and a worker that failed to PARSE an error would report
// the parse instead of the refusal — which is strictly less useful than the bare
// status it already had.
func refusalDetail(body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return env.Error
	}
	if s := strings.TrimSpace(string(body)); s != "" && len(s) <= 512 {
		return s
	}
	return ""
}
