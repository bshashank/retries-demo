package sim

import (
	"math"
	"sort"
	"sync"
	"time"
)

// completedSample is one job that a worker actually ran to a conclusion, whether
// it succeeded, errored, or was degraded by a non-essential dependency.
type completedSample struct {
	at        time.Time
	latency   time.Duration // end-to-end: enqueue -> result (queue wait included)
	queueWait time.Duration
	errored   bool
	degraded  bool
}

// stampedDuration is a queue-wait observation taken at dequeue time.
type stampedDuration struct {
	at time.Time
	d  time.Duration
}

// metrics is a per-node rolling window of observations. Everything is recorded
// under one mutex with the timestamp taken *inside* the lock, which keeps each
// slice strictly ordered by time and makes pruning a cheap prefix trim.
//
// Windows are pruned on read (once per engine tick) with a size-based safety
// valve on write, so a node that stops being read cannot grow without bound.
type metrics struct {
	window time.Duration

	mu        sync.Mutex
	now       func() time.Time // injectable for deterministic tests
	completed []completedSample
	waits     []stampedDuration
	rejected  []time.Time
	abandoned []time.Time
}

// pruneHighWater is the per-slice length above which a writer prunes eagerly.
const pruneHighWater = 4096

func newMetrics(window time.Duration) *metrics {
	return &metrics{window: window, now: time.Now}
}

// RecordCompleted records a job a worker ran to completion. latency is measured
// from enqueue so it is what a caller actually experienced, not just service time.
func (m *metrics) RecordCompleted(latency, queueWait time.Duration, errored, degraded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.completed = append(m.completed, completedSample{
		at:        now,
		latency:   latency,
		queueWait: queueWait,
		errored:   errored,
		degraded:  degraded,
	})
	if len(m.completed) > pruneHighWater {
		m.pruneLocked(now)
	}
}

// RecordQueueWait records how long a job sat in the queue before a worker picked
// it up. Taken at dequeue rather than at completion so a backing-up node shows
// pressure immediately instead of one service time later.
func (m *metrics) RecordQueueWait(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.waits = append(m.waits, stampedDuration{at: now, d: d})
	if len(m.waits) > pruneHighWater {
		m.pruneLocked(now)
	}
}

// RecordRejected records load shed because the queue was full.
func (m *metrics) RecordRejected() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.rejected = append(m.rejected, now)
	if len(m.rejected) > pruneHighWater {
		m.pruneLocked(now)
	}
}

// RecordAbandoned records a job dropped at dequeue because its caller had
// already given up. Cancellation propagation is real behaviour worth surfacing:
// a node that is mostly abandoning work is failing even if it never errors.
func (m *metrics) RecordAbandoned() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.abandoned = append(m.abandoned, now)
	if len(m.abandoned) > pruneHighWater {
		m.pruneLocked(now)
	}
}

func (m *metrics) pruneLocked(now time.Time) {
	cutoff := now.Add(-m.window)

	i := 0
	for i < len(m.completed) && m.completed[i].at.Before(cutoff) {
		i++
	}
	m.completed = append(m.completed[:0], m.completed[i:]...)

	i = 0
	for i < len(m.waits) && m.waits[i].at.Before(cutoff) {
		i++
	}
	m.waits = append(m.waits[:0], m.waits[i:]...)

	m.rejected = pruneTimes(m.rejected, cutoff)
	m.abandoned = pruneTimes(m.abandoned, cutoff)
}

func pruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return append(ts[:0], ts[i:]...)
}

// metricsView is a point-in-time reduction of the rolling window.
type metricsView struct {
	Completed     int
	Rejected      int
	Abandoned     int
	Degraded      int
	Errored       int
	TotalAttempts int

	ThroughputPerSec float64
	ErrorRate        float64
	RejectRate       float64
	AbandonRate      float64
	DegradedRate     float64

	MeanQueueWaitMs float64
	P50LatencyMs    float64
	P95LatencyMs    float64
}

// Read prunes the window to now and reduces it.
//
// Rate denominators are total *attempts* (completed + rejected + abandoned), not
// completions. Using completions would be self-flattering: a node that sheds 90%
// of its load would report a 0% error rate.
func (m *metrics) Read(now time.Time) metricsView {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)

	v := metricsView{
		Completed: len(m.completed),
		Rejected:  len(m.rejected),
		Abandoned: len(m.abandoned),
	}
	v.TotalAttempts = v.Completed + v.Rejected + v.Abandoned

	latencies := make([]float64, 0, len(m.completed))
	for _, s := range m.completed {
		if s.errored {
			v.Errored++
		}
		if s.degraded {
			v.Degraded++
		}
		latencies = append(latencies, float64(s.latency)/float64(time.Millisecond))
	}

	windowSec := m.window.Seconds()
	if windowSec > 0 {
		v.ThroughputPerSec = float64(v.Completed) / windowSec
	}

	if v.TotalAttempts > 0 {
		den := float64(v.TotalAttempts)
		v.ErrorRate = float64(v.Errored) / den
		v.RejectRate = float64(v.Rejected) / den
		v.AbandonRate = float64(v.Abandoned) / den
	}
	if v.Completed > 0 {
		v.DegradedRate = float64(v.Degraded) / float64(v.Completed)
	}

	if n := len(m.waits); n > 0 {
		var total time.Duration
		for _, w := range m.waits {
			total += w.d
		}
		v.MeanQueueWaitMs = float64(total) / float64(n) / float64(time.Millisecond)
	}

	sort.Float64s(latencies)
	v.P50LatencyMs = percentile(latencies, 0.50)
	v.P95LatencyMs = percentile(latencies, 0.95)
	return v
}

// percentile is nearest-rank on an ascending slice: the smallest value at or
// above the p-th position. Returns 0 for an empty sample.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// runSample is one whole pipeline run measured at the orchestrator entry point.
type runSample struct {
	at      time.Time
	latency time.Duration
	ok      bool
}

// runMetrics is the run-level rolling window fed by the load generator.
type runMetrics struct {
	window time.Duration

	mu      sync.Mutex
	now     func() time.Time
	samples []runSample
}

func newRunMetrics(window time.Duration) *runMetrics {
	return &runMetrics{window: window, now: time.Now}
}

func (r *runMetrics) Record(latency time.Duration, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.samples = append(r.samples, runSample{at: now, latency: latency, ok: ok})
	if len(r.samples) > pruneHighWater {
		r.pruneLocked(now)
	}
}

func (r *runMetrics) pruneLocked(now time.Time) {
	cutoff := now.Add(-r.window)
	i := 0
	for i < len(r.samples) && r.samples[i].at.Before(cutoff) {
		i++
	}
	r.samples = append(r.samples[:0], r.samples[i:]...)
}

type runView struct {
	RunsPerSec  float64
	SuccessRate float64
	P95Ms       float64
}

func (r *runMetrics) Read(now time.Time) runView {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)

	var v runView
	n := len(r.samples)
	if n == 0 {
		return v
	}
	okCount := 0
	latencies := make([]float64, 0, n)
	for _, s := range r.samples {
		if s.ok {
			okCount++
		}
		latencies = append(latencies, float64(s.latency)/float64(time.Millisecond))
	}
	if sec := r.window.Seconds(); sec > 0 {
		v.RunsPerSec = float64(n) / sec
	}
	v.SuccessRate = float64(okCount) / float64(n)
	sort.Float64s(latencies)
	v.P95Ms = percentile(latencies, 0.95)
	return v
}
