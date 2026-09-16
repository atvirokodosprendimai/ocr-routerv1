package router

// Counter is the slice of the metrics registry the single writer needs.
//
// An interface at the CONSUMER rather than an import of the monitor package:
// router is the lower layer, and it must not depend on how — or whether —
// anything is being observed. It also means a Service built without metrics
// still works, which is what keeps every existing test unchanged.
type Counter interface {
	Inc(name string, labels map[string]string)
	Add(name string, labels map[string]string, n int64)
}

// nopCounter is the default, so no call site needs a nil check.
//
// ⚠ This is the one place a metric can be silently lost. The default is chosen
// deliberately over a nil check at each call site — thirty guarded call sites is
// thirty chances to forget one — and T11's tests assert the counters MOVE
// through the real registry, so a Service wired with the nop in production would
// show as flat lines rather than as a crash.
type nopCounter struct{}

func (nopCounter) Inc(string, map[string]string)        {}
func (nopCounter) Add(string, map[string]string, int64) {}

// Metric names the service increments. They are duplicated from the monitor
// package's constants rather than imported, because importing upward would
// invert the dependency; the pair is pinned by a test in T11.
const (
	metricJobsTotal     = "ocrr_jobs_total"
	metricStageAdvances = "ocrr_stage_advances_total"
	// metricRawJobs counts raw jobs DELIVERED, by label. Raw output is priced and
	// transported unlike anything else here, so "jobs delivered" alone cannot
	// show a raw service growing. Label is the only dimension, which keeps it
	// inside T11's cardinality allow-list rather than beside it.
	metricRawJobs        = "ocrr_raw_jobs_total"
	metricReaperActions  = "ocrr_reaper_actions_total"
	metricCreditsDebited = "ocrr_credits_debited_total"
)

// SetCounter attaches a metrics registry to the service.
func (s *Service) SetCounter(c Counter) {
	if c == nil {
		c = nopCounter{}
	}
	s.counter = c
}
