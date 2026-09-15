// Package monitor makes the router's health machine-readable.
//
// Two endpoints with deliberately different exposures: /healthz answers "can
// this process still do its job" on the public listener, and /metrics carries
// the operational numbers on a loopback-bound listener of its own.
package monitor

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// allowedLabelNames is the complete set of metric label NAMES this process may
// emit.
//
// ⚠ THIS IS THE CARDINALITY GUARD, and it lives in code rather than in a comment
// because a prose rule about cardinality is invisible to the person adding the
// next field. Every value here is drawn from a bounded set: a service label
// (operator-controlled, since a client cannot mint one — an unknown label is
// refused at upload), a job state (a fixed enum), a reaper action (a fixed
// enum).
//
// A user id, job id or email would be UNBOUNDED, and unbounded label values are
// how a metrics endpoint kills the scraper it feeds. Per-customer visibility
// belongs on the dashboard, which is authenticated and paginated.
var allowedLabelNames = map[string]bool{
	"label":  true,
	"state":  true,
	"action": true,
}

// Registry holds the counters. Gauges are not stored: they are computed at
// scrape time from the read handle, so the scrape pays for them rather than
// every upload.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
	// order keeps a stable rendering order, because a scrape that reshuffles
	// its own output is needlessly hard to diff by eye.
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: map[string]*atomic.Int64{}}
}

// key renders a metric name and its labels into a series key.
//
// It PANICS on a label name outside the allow-list. A panic is right here: an
// unbounded label is a programming error that must never reach production, and
// the tests exercise every registration path, so it surfaces in CI rather than
// in a scrape.
func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		if !allowedLabelNames[k] {
			panic(fmt.Sprintf(
				"monitor: metric label %q is not allow-listed; unbounded label values kill the "+
					"scraper. Allowed: label, state, action", k))
		}
		names = append(names, k)
	}
	sort.Strings(names)

	out := name + "{"
	for i, k := range names {
		if i > 0 {
			out += ","
		}
		out += k + `="` + escapeLabelValue(labels[k]) + `"`
	}
	return out + "}"
}

// escapeLabelValue applies the Prometheus text-format escaping rules.
func escapeLabelValue(v string) string {
	out := make([]rune, 0, len(v))
	for _, r := range v {
		switch r {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// Inc adds one to a counter series, creating it on first use.
func (r *Registry) Inc(name string, labels map[string]string) {
	r.Add(name, labels, 1)
}

// Add adds n to a counter series.
func (r *Registry) Add(name string, labels map[string]string, n int64) {
	k := key(name, labels)

	r.mu.Lock()
	c, ok := r.counters[k]
	if !ok {
		c = &atomic.Int64{}
		r.counters[k] = c
		r.order = append(r.order, k)
		sort.Strings(r.order)
	}
	r.mu.Unlock()

	c.Add(n)
}

// Value reads one counter series. Tests only — a scrape renders everything.
func (r *Registry) Value(name string, labels map[string]string) int64 {
	k := key(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[k]; ok {
		return c.Load()
	}
	return 0
}

// snapshot returns the counter series in stable order.
func (r *Registry) snapshot() []series {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]series, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, series{key: k, value: r.counters[k].Load()})
	}
	return out
}

type series struct {
	key   string
	value int64
}

// Metric names. Each is here because it answers a question an operator actually
// has at 3am, and the two marked are the ones that earn the endpoint.
const (
	// MetricJobsTotal counts terminal outcomes by state.
	MetricJobsTotal = "ocrr_jobs_total"
	// MetricStageAdvances counts pipeline movement — a pipeline that stops
	// advancing looks exactly like one that is merely slow.
	MetricStageAdvances = "ocrr_stage_advances_total"
	// MetricReaperActions counts requeues, expiries and sweeps, so a reaper that
	// silently stopped is visible.
	MetricReaperActions = "ocrr_reaper_actions_total"
	// MetricCreditsDebited counts credits charged, which should agree with the
	// ledger.
	MetricCreditsDebited = "ocrr_credits_debited_total"

	// ★ MetricWorkersLive is THE silent failure: a label with no worker queues
	// until its jobs hit their deadline, and nothing else reports it.
	MetricWorkersLive = "ocrr_workers_live"
	// ★ MetricQueueOldestAge is the better stall signal than depth — depth can
	// sit low while one job is wedged forever.
	MetricQueueOldestAge = "ocrr_queue_oldest_age_seconds"
	// MetricQueueDepth is the backlog size per label.
	MetricQueueDepth = "ocrr_queue_depth"
	// MetricResultsInMemory tracks the unbounded-growth risk in the result store.
	MetricResultsInMemory = "ocrr_results_in_memory"
)
