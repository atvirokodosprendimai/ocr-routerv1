package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// healthResponse is what /healthz answers.
type healthResponse struct {
	OK      bool     `json:"ok"`
	Version string   `json:"version,omitempty"`
	Failed  []string `json:"failed,omitempty"`
}

// HealthHandler probes the process's own dependencies.
//
// ⚠ IT DOES REAL WORK. A handler that returns 200 unconditionally is the classic
// vacuous gate, and it is WORSE than no health check at all, because it is
// trusted: every probe stays green through a total outage. This one runs a
// SELECT 1 against the read handle and creates-writes-removes a file in the blob
// directory, and the tests break each of those in turn.
//
// ⚠ IT DELIBERATELY IGNORES WORKER AVAILABILITY. Zero workers for a label means
// the queue is filling and THE ROUTER IS FINE. If this reported that as
// unhealthy, an orchestrator would restart the one component still working, and
// the restart would fix nothing — so it would do it again. Degraded and dead are
// different states and only one of them is a liveness question; worker
// availability is ocrr_workers_live and an alert.
func (m *Monitor) HealthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var failed []string

	if err := m.deps.PingDB(ctx); err != nil {
		failed = append(failed, "db")
	}
	if err := probeWritable(m.deps.BlobDir); err != nil {
		failed = append(failed, "blobs")
	}

	body := healthResponse{OK: len(failed) == 0, Version: m.deps.Version, Failed: failed}
	status := http.StatusOK
	if !body.OK {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// probeWritable confirms the blob volume still accepts writes.
//
// A full or read-only volume is the failure this catches, and it is one that a
// SELECT would not: the database can be perfectly healthy while every upload is
// about to fail.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".healthz-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()

	// Actually write: creating a file can succeed on a volume that is out of
	// space, and the write is where it fails.
	if _, err := f.Write([]byte("ok")); err != nil {
		return err
	}
	return f.Sync()
}
