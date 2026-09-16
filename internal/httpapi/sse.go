package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/atvirokodosprendimai/ocr-router/internal/bus"
	"github.com/atvirokodosprendimai/ocr-router/internal/core"
)

// handleSSE is the command channel: the router tells a client which files are
// ready, and tells workers that work exists.
//
// One connection per caller, server-controlled cadence, one select loop.
func (a *API) handleSSE(w http.ResponseWriter, r *http.Request) {
	p := principal(r)

	// ⚠ CLEAR THE WRITE DEADLINE FIRST, before anything is written.
	//
	// http.Server.WriteTimeout applies to the whole response, and for a stream
	// that never ends it is a guillotine: the connection dies mid-session with
	// nothing in the logs and nothing on the wire to explain it. Clearing it per
	// stream is the only thing that makes a long-lived SSE connection safe, and
	// no amount of correct handler logic substitutes for it.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	var topic string
	switch p.Role {
	case core.RoleClient, core.RoleAdmin:
		topic = bus.UserTopic(p.UserID)
	case core.RoleWorker:
		label := r.URL.Query().Get("label")
		if label == "" {
			writeError(w, core.ErrInvalidParam)
			return
		}
		// Declaring the mode on the SUBSCRIBE too, not only on claim. This is
		// the call that registers the label at all, so a worker refused on
		// /claim but accepted here would still make its label available to
		// clients — advertising a service nothing can correctly serve.
		raw, err := rawParam(r)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := a.deps.Router.CheckWorkerMode(r.Context(), label, raw); err != nil {
			writeError(w, err)
			return
		}
		topic = bus.WorkerTopic(label)
	default:
		writeError(w, core.ErrForbidden)
		return
	}

	events, unsubscribe := a.deps.Bus.Subscribe(topic)
	// Subscribing BEFORE sending the backlog closes the gap where a job finishes
	// after the backlog is computed but before the subscription exists — the
	// client would otherwise never hear about it and would wait forever.
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer will hold an event-stream indefinitely; this is the
	// conventional opt-out and is harmless where it is not understood.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)

	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flush(w)
		return true
	}

	if !send("hello", map[string]any{"role": p.Role, "user_id": p.UserID}) {
		return
	}

	// The backlog is what makes a dropped client self-healing: it does not need
	// to have been listening when its job finished.
	if p.Role == core.RoleClient {
		items, err := a.deps.Router.Backlog(r.Context(), p.UserID)
		if err == nil && !send("backlog", map[string]any{"jobs": items}) {
			return
		}
	}

	ticker := time.NewTicker(a.deps.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-events:
			if !ok {
				return
			}
			if !send(string(e.Kind), sseEvent{
				JobID:  e.JobID,
				Units:  e.Units,
				Reason: e.Reason,
				Label:  e.Label,
			}) {
				return
			}

		case <-ticker.C:
			// The keepalive does two jobs: it stops an idle connection being
			// reaped by a proxy, and it gives the client a positive signal that
			// the stream is alive rather than merely silent. The worker agent
			// also treats it as a cue to poll, so a dropped `work` event costs
			// one ping interval instead of stalling until the next upload.
			if !send("ping", map[string]any{"t": a.deps.Now().Unix()}) {
				return
			}
		}
	}
}

// sseEvent is the wire shape of a command.
//
// Ids and scalars only — never the result itself. The reader fetches, which is
// what makes two events racing for one job resolve correctly.
type sseEvent struct {
	JobID  string `json:"job_id,omitempty"`
	Units  int    `json:"units,omitempty"`
	Reason string `json:"reason,omitempty"`
	Label  string `json:"label,omitempty"`
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
