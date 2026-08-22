package sim

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fakeClock lets the window tests move time without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestMetrics(window time.Duration) (*metrics, *fakeClock) {
	c := newFakeClock()
	m := newMetrics(window)
	m.now = c.Now
	return m, c
}

func TestMetricsEmptyIsZeroAndSafe(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)
	v := m.Read(c.Now())

	if v.TotalAttempts != 0 {
		t.Fatalf("TotalAttempts = %d, want 0", v.TotalAttempts)
	}
	// Every derived rate divides by a denominator that is zero here.
	for name, got := range map[string]float64{
		"errorRate":     v.ErrorRate,
		"rejectRate":    v.RejectRate,
		"abandonRate":   v.AbandonRate,
		"degradedRate":  v.DegradedRate,
		"throughput":    v.ThroughputPerSec,
		"meanQueueWait": v.MeanQueueWaitMs,
		"p50":           v.P50LatencyMs,
		"p95":           v.P95LatencyMs,
	} {
		if got != 0 || math.IsNaN(got) {
			t.Errorf("%s = %v, want a clean 0 (no NaN/Inf from empty windows)", name, got)
		}
	}
}

func TestMetricsWindowPruning(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)

	// Ten samples, one per second, spread over the boundary.
	for i := 0; i < 10; i++ {
		m.RecordCompleted(100*time.Millisecond, 10*time.Millisecond, false, false)
		m.RecordQueueWait(10 * time.Millisecond)
		m.RecordRejected()
		m.RecordAbandoned()
		c.Advance(time.Second)
	}

	// Now at t+10s: only samples from t+5s onward survive (5 of 10).
	v := m.Read(c.Now())
	if v.Completed != 5 {
		t.Errorf("completed in window = %d, want 5", v.Completed)
	}
	if v.Rejected != 5 {
		t.Errorf("rejected in window = %d, want 5", v.Rejected)
	}
	if v.Abandoned != 5 {
		t.Errorf("abandoned in window = %d, want 5", v.Abandoned)
	}
	if v.MeanQueueWaitMs != 10 {
		t.Errorf("meanQueueWaitMs = %v, want 10", v.MeanQueueWaitMs)
	}

	// Walk past every sample and the window empties completely.
	c.Advance(30 * time.Second)
	v = m.Read(c.Now())
	if v.TotalAttempts != 0 {
		t.Fatalf("stale samples survived pruning: %+v", v)
	}
	if v.MeanQueueWaitMs != 0 || v.P95LatencyMs != 0 {
		t.Fatalf("derived values not cleared after pruning: %+v", v)
	}

	// And the backing slices really were trimmed, not just ignored.
	m.mu.Lock()
	n := len(m.completed) + len(m.waits) + len(m.rejected) + len(m.abandoned)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("pruning left %d samples retained", n)
	}
}

func TestMetricsRateDenominatorIncludesRejectedAndAbandoned(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)

	// 5 clean completions, 3 rejections, 2 abandonments: 10 attempts.
	for i := 0; i < 5; i++ {
		m.RecordCompleted(50*time.Millisecond, 0, false, false)
	}
	for i := 0; i < 3; i++ {
		m.RecordRejected()
	}
	for i := 0; i < 2; i++ {
		m.RecordAbandoned()
	}

	v := m.Read(c.Now())
	if v.TotalAttempts != 10 {
		t.Fatalf("TotalAttempts = %d, want 10 (completed+rejected+abandoned)", v.TotalAttempts)
	}
	if v.RejectRate != 0.3 {
		t.Errorf("RejectRate = %v, want 0.3", v.RejectRate)
	}
	if v.AbandonRate != 0.2 {
		t.Errorf("AbandonRate = %v, want 0.2", v.AbandonRate)
	}
	// Zero errors out of ten attempts, not out of five completions.
	if v.ErrorRate != 0 {
		t.Errorf("ErrorRate = %v, want 0", v.ErrorRate)
	}

	// Add errors and confirm the denominator stays at attempts.
	m2, c2 := newTestMetrics(5 * time.Second)
	for i := 0; i < 5; i++ {
		m2.RecordCompleted(50*time.Millisecond, 0, true, false)
	}
	for i := 0; i < 5; i++ {
		m2.RecordRejected()
	}
	v2 := m2.Read(c2.Now())
	if v2.ErrorRate != 0.5 {
		t.Fatalf("ErrorRate = %v, want 0.5 (5 errors / 10 attempts, not 5/5)", v2.ErrorRate)
	}
}

func TestMetricsThroughputAndDegraded(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)

	for i := 0; i < 100; i++ {
		m.RecordCompleted(10*time.Millisecond, 0, false, i < 25)
	}
	v := m.Read(c.Now())
	if v.ThroughputPerSec != 20 {
		t.Errorf("ThroughputPerSec = %v, want 20 (100 completions / 5s window)", v.ThroughputPerSec)
	}
	if v.Degraded != 25 || v.DegradedRate != 0.25 {
		t.Errorf("degraded = %d rate = %v, want 25 / 0.25", v.Degraded, v.DegradedRate)
	}
}

func TestPercentileKnownInput(t *testing.T) {
	t.Parallel()

	// 1..100 ms. Nearest rank: p50 -> 50th value, p95 -> 95th value.
	vals := make([]float64, 100)
	for i := range vals {
		vals[i] = float64(i + 1)
	}
	if got := percentile(vals, 0.50); got != 50 {
		t.Errorf("p50 = %v, want 50", got)
	}
	if got := percentile(vals, 0.95); got != 95 {
		t.Errorf("p95 = %v, want 95", got)
	}
	if got := percentile(vals, 1.0); got != 100 {
		t.Errorf("p100 = %v, want 100", got)
	}
	if got := percentile(vals, 0); got != 1 {
		t.Errorf("p0 = %v, want 1", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("p95 of empty = %v, want 0", got)
	}
	if got := percentile([]float64{7}, 0.95); got != 7 {
		t.Errorf("p95 of single sample = %v, want 7", got)
	}
	// Small sample: 10 values, p95 -> ceil(9.5)-1 = index 9 -> the max.
	small := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(small, 0.95); got != 10 {
		t.Errorf("p95 of 10 samples = %v, want 10", got)
	}
	if got := percentile(small, 0.50); got != 5 {
		t.Errorf("p50 of 10 samples = %v, want 5", got)
	}
}

func TestMetricsPercentilesFromWindow(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)

	// Insert out of order to prove the reducer sorts.
	for _, ms := range []int{100, 5, 50, 1000, 20, 10, 30, 40, 60, 70} {
		m.RecordCompleted(time.Duration(ms)*time.Millisecond, 0, false, false)
	}
	v := m.Read(c.Now())
	// sorted: 5 10 20 30 40 50 60 70 100 1000 -> p50 idx4 = 40, p95 idx9 = 1000
	if v.P50LatencyMs != 40 {
		t.Errorf("p50 = %v, want 40", v.P50LatencyMs)
	}
	if v.P95LatencyMs != 1000 {
		t.Errorf("p95 = %v, want 1000", v.P95LatencyMs)
	}
}

func TestMetricsMeanQueueWait(t *testing.T) {
	t.Parallel()
	m, c := newTestMetrics(5 * time.Second)

	m.RecordQueueWait(100 * time.Millisecond)
	m.RecordQueueWait(200 * time.Millisecond)
	m.RecordQueueWait(300 * time.Millisecond)

	v := m.Read(c.Now())
	if v.MeanQueueWaitMs != 200 {
		t.Fatalf("meanQueueWaitMs = %v, want 200", v.MeanQueueWaitMs)
	}
}

func TestMetricsConcurrentRecording(t *testing.T) {
	t.Parallel()
	m := newMetrics(5 * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.RecordCompleted(time.Millisecond, time.Millisecond, j%3 == 0, j%5 == 0)
				m.RecordQueueWait(time.Millisecond)
				if j%7 == 0 {
					m.RecordRejected()
				}
				if j%11 == 0 {
					m.RecordAbandoned()
				}
			}
		}()
	}
	// Read concurrently with the writers: this is exactly what the tick loop does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = m.Read(time.Now())
		}
	}()
	wg.Wait()

	v := m.Read(time.Now())
	if v.Completed != 16*200 {
		t.Fatalf("completed = %d, want %d (lost samples under concurrency)", v.Completed, 16*200)
	}
}

func TestRunMetrics(t *testing.T) {
	t.Parallel()

	c := newFakeClock()
	r := newRunMetrics(5 * time.Second)
	r.now = c.Now

	if v := r.Read(c.Now()); v.RunsPerSec != 0 || v.SuccessRate != 0 || v.P95Ms != 0 {
		t.Fatalf("empty run metrics = %+v, want zeroes", v)
	}

	for i := 0; i < 100; i++ {
		r.Record(time.Duration(i+1)*time.Millisecond, i < 90)
	}
	v := r.Read(c.Now())
	if v.RunsPerSec != 20 {
		t.Errorf("RunsPerSec = %v, want 20", v.RunsPerSec)
	}
	if v.SuccessRate != 0.9 {
		t.Errorf("SuccessRate = %v, want 0.9", v.SuccessRate)
	}
	if v.P95Ms != 95 {
		t.Errorf("P95Ms = %v, want 95", v.P95Ms)
	}

	c.Advance(10 * time.Second)
	if v := r.Read(c.Now()); v.RunsPerSec != 0 {
		t.Fatalf("stale run samples survived pruning: %+v", v)
	}
}

func TestRoundHelpersAreNaNSafe(t *testing.T) {
	t.Parallel()
	if got := round2(math.NaN()); got != 0 {
		t.Errorf("round2(NaN) = %v, want 0", got)
	}
	if got := round4(math.Inf(1)); got != 0 {
		t.Errorf("round4(+Inf) = %v, want 0", got)
	}
	if got := round2(1.23456); got != 1.23 {
		t.Errorf("round2(1.23456) = %v, want 1.23", got)
	}
	if got := round4(0.123456); got != 0.1235 {
		t.Errorf("round4(0.123456) = %v, want 0.1235", got)
	}
}
