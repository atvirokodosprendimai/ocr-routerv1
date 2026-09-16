package monitor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"
)

// Deps is what the monitor reads. Each is a narrow function rather than a whole
// service, so the monitor cannot accidentally mutate anything it observes.
type Deps struct {
	Version string
	BlobDir string

	// PingDB runs a trivial query through the READ handle. Taking a func rather
	// than the *sql.DB keeps the monitor unable to write even by mistake.
	PingDB func(ctx context.Context) error

	// QueueDepth and OldestQueued are computed at scrape time.
	QueueDepth   func(ctx context.Context) (map[string]int, error)
	OldestQueued func(ctx context.Context) (map[string]time.Time, error)
	// JobStates is the census by state.
	JobStates func(ctx context.Context) (map[string]int, error)
	// LiveLabels and WorkersFor answer the silent-failure question.
	LiveLabels func(now time.Time) []string
	WorkersFor func(label string) int
	// ResultsInMemory is the size of the result store.
	ResultsInMemory func() int
	// RawResultsPending is how many raw results are on disk awaiting collection.
	// Optional: nil means the sample is omitted rather than reported as zero,
	// because "not measured" and "none" are different claims.
	RawResultsPending func() int

	Now func() time.Time
}

// Monitor serves /healthz and /metrics.
type Monitor struct {
	deps Deps
	reg  *Registry
}

// New builds a monitor over a registry.
func New(reg *Registry, deps Deps) *Monitor {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Monitor{deps: deps, reg: reg}
}

// Registry returns the counter registry, so the write paths can increment it.
func (m *Monitor) Registry() *Registry { return m.reg }

// MetricsHandler renders Prometheus text format.
//
// Gauges are computed HERE, on the scrape, rather than maintained on every
// write. The scrape pays the cost, not the upload path — and a gauge derived
// from the database on demand cannot drift from it, which a maintained counter
// can.
func (m *Monitor) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var b bytesBuilder
	now := m.deps.Now()

	// ★ The silent failure first: which labels have nobody serving them.
	labels := map[string]struct{}{}
	for _, l := range m.deps.LiveLabels(now) {
		labels[l] = struct{}{}
	}
	depth, err := m.deps.QueueDepth(ctx)
	if err == nil {
		for l := range depth {
			labels[l] = struct{}{}
		}
	}
	oldest, _ := m.deps.OldestQueued(ctx)

	ordered := make([]string, 0, len(labels))
	for l := range labels {
		ordered = append(ordered, l)
	}
	sort.Strings(ordered)

	b.help(MetricWorkersLive, "gauge", "Workers currently serving each service label.")
	for _, l := range ordered {
		b.sample(MetricWorkersLive, map[string]string{"label": l}, float64(m.deps.WorkersFor(l)))
	}

	b.help(MetricQueueDepth, "gauge", "Queued jobs per service label.")
	for _, l := range ordered {
		b.sample(MetricQueueDepth, map[string]string{"label": l}, float64(depth[l]))
	}

	b.help(MetricQueueOldestAge, "gauge",
		"Age in seconds of the oldest queued job per label. A better stall signal than depth.")
	for _, l := range ordered {
		age := 0.0
		if t, ok := oldest[l]; ok {
			age = now.Sub(t).Seconds()
		}
		b.sample(MetricQueueOldestAge, map[string]string{"label": l}, age)
	}

	if states, err := m.deps.JobStates(ctx); err == nil {
		b.help("ocrr_jobs_current", "gauge", "Jobs currently in each state.")
		keys := make([]string, 0, len(states))
		for s := range states {
			keys = append(keys, s)
		}
		sort.Strings(keys)
		for _, s := range keys {
			b.sample("ocrr_jobs_current", map[string]string{"state": s}, float64(states[s]))
		}
	}

	b.help(MetricResultsInMemory, "gauge", "Results held in memory awaiting collection.")
	b.sample(MetricResultsInMemory, nil, float64(m.deps.ResultsInMemory()))
	if m.deps.RawResultsPending != nil {
		b.help(MetricRawResultsPending, "gauge", "Raw results on disk awaiting collection.")
		b.sample(MetricRawResultsPending, nil, float64(m.deps.RawResultsPending()))
	}

	// Counters, rendered from the registry.
	for _, s := range m.reg.snapshot() {
		b.raw(s.key, float64(s.value))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.bytes())
}

// Serve runs the metrics listener.
//
// ⚠ A SEPARATE LISTENER, bound to loopback by default, and that is the security
// decision rather than a deployment detail. Queue depths and throughput say how
// much work a customer is pushing, which is commercially sensitive, and the main
// listener faces the internet.
//
// The obvious alternative — /metrics on the main listener behind the admin token
// — works, but it puts a credential that CREATES USERS AND MINTS TOKENS into a
// scrape config, which is the least-guarded file in most deployments. A separate
// bind gives the same protection with no credential at all, and exposing it
// becomes a deliberate act in the operator's proxy.
// It returns the address the listener ACTUALLY bound to. That return value is
// not a convenience: a test asserting the flag's default *string* proves nothing
// about the socket, and this is what lets the loopback default be asserted
// against the thing that is really listening.
func (m *Monitor) Serve(ctx context.Context, addr string) (string, func(context.Context) error, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.MetricsHandler)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("metrics listener on %s: %w", addr, err)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	return ln.Addr().String(), srv.Shutdown, nil
}

// bytesBuilder renders the text format.
type bytesBuilder struct{ b []byte }

func (x *bytesBuilder) help(name, typ, help string) {
	x.b = append(x.b, "# HELP "+name+" "+help+"\n"...)
	x.b = append(x.b, "# TYPE "+name+" "+typ+"\n"...)
}

func (x *bytesBuilder) sample(name string, labels map[string]string, v float64) {
	x.raw(key(name, labels), v)
}

func (x *bytesBuilder) raw(seriesKey string, v float64) {
	x.b = append(x.b, fmt.Sprintf("%s %g\n", seriesKey, v)...)
}

func (x *bytesBuilder) bytes() []byte { return x.b }
