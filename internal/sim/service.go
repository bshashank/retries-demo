package sim

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Errors surfaced by the simulated services.
var (
	// ErrQueueFull is returned when a service sheds load rather than blocking a
	// caller on a full queue.
	ErrQueueFull = errors.New("sim: queue full (load shed)")

	// ErrInjected is returned when a node's injected failure probability fires.
	ErrInjected = errors.New("sim: injected failure")
)

// Result is the outcome of one call into a service.
//
// Err is a hard failure. Degraded is the soft signal: the call succeeded, but
// something non-essential underneath it did not. Keeping them as separate
// fields is the whole reason a non-essential outage can be visible without
// being fatal.
type Result struct {
	Err      error
	Degraded bool
}

// job is one unit of work sitting in a service's queue.
type job struct {
	Ctx        context.Context
	EnqueuedAt time.Time

	// Done is buffered with capacity 1 so a worker can always deliver its result
	// and move on, even if the caller timed out and stopped listening. An
	// unbuffered channel here would park the worker forever on every timed-out
	// call: a goroutine leak that compounds exactly when the system is already
	// under stress.
	Done chan Result
}

// atomicFloat is a lock-free float64 cell, used for the runtime-tunable
// injection knobs that are read on every single call.
type atomicFloat struct {
	bits atomic.Uint64
}

func (a *atomicFloat) Store(v float64) { a.bits.Store(math.Float64bits(v)) }
func (a *atomicFloat) Load() float64   { return math.Float64frombits(a.bits.Load()) }

// atomicMode is a lock-free DependencyMode cell, the same pattern as
// atomicFloat above for the other per-call knobs that must stay readable
// while an operator is toggling them mid-flight.
type atomicMode struct {
	v atomic.Value
}

func (a *atomicMode) Store(m DependencyMode) { a.v.Store(m) }
func (a *atomicMode) Load() DependencyMode   { return a.v.Load().(DependencyMode) }

// dependency is one outgoing edge. Mode is an atomicMode because operators
// can reclassify an edge while calls are in flight.
type dependency struct {
	from  string
	to    string
	child *service

	mode        atomicMode
	defaultMode DependencyMode
	timeout     time.Duration
}

// service is one simulated node: a buffered channel for a queue and a fixed
// pool of worker goroutines draining it. There is no simulated queueing model
// here; the queue is a real Go channel and the contention is real contention.
type service struct {
	id          string
	label       string
	tier        int
	baseLatency time.Duration
	workerCount int

	queue chan *job
	deps  []*dependency

	// gateHoldBudget > 0 marks this service as a gated resource (SAST Engine,
	// Container Registry) and is the deadline a call gets once gatedCall
	// promotes it to a detached background attempt. Zero means "not a gate."
	gateHoldBudget time.Duration

	// shedding is a hysteresis latch for gatedCall's admission check: true once
	// occupancy crosses gateAdmitFraction, held true until occupancy drains back
	// down to gateResumeFraction. Without it, a hard threshold on a live queue
	// length oscillates every tick right at the boundary — shedding drains the
	// queue just enough to stop shedding, which lets it refill just enough to
	// start again. tick() also reads this to derive the gate's own FAILING
	// status, so "the banner reads FAILING" and "shedding is happening" report
	// the exact same debounced signal instead of two thresholds that can
	// oscillate out of phase with each other.
	shedding atomic.Bool

	latencyMultiplier atomicFloat
	failRate          atomicFloat
	inFlight          atomic.Int64

	metrics *metrics
	wg      sync.WaitGroup
}

func newService(spec nodeSpec) *service {
	s := &service{
		id:             spec.ID,
		label:          spec.Label,
		tier:           spec.Tier,
		baseLatency:    spec.BaseLatency,
		workerCount:    spec.Workers,
		queue:          make(chan *job, spec.QueueCap),
		gateHoldBudget: spec.GateHoldBudget,
		metrics:        newMetrics(metricsWindow),
	}
	s.latencyMultiplier.Store(1)
	s.failRate.Store(0)
	return s
}

// start launches the worker pool.
func (s *service) start() {
	s.wg.Add(s.workerCount)
	for i := 0; i < s.workerCount; i++ {
		go s.worker()
	}
}

// shutdown closes the queue and waits for the pool to drain. It is only ever
// called after every possible caller of this service has already stopped, which
// the engine guarantees by shutting down in topological order.
func (s *service) shutdown() {
	close(s.queue)
	s.wg.Wait()
}

// call enqueues work and waits for the result.
//
// Enqueue never blocks: a full queue sheds immediately. Blocking on a full queue
// is how a slow leaf silently converts into a stalled caller, and refusing to do
// it is what makes backpressure visible as a rejection rate instead of an
// invisible stall.
func (s *service) call(ctx context.Context) Result {
	j := &job{
		Ctx:        ctx,
		EnqueuedAt: time.Now(),
		Done:       make(chan Result, 1),
	}

	select {
	case s.queue <- j:
	default:
		s.metrics.RecordRejected()
		return Result{Err: ErrQueueFull}
	}

	select {
	case r := <-j.Done:
		return r
	case <-ctx.Done():
		// The caller gives up; the job stays queued and will be counted as
		// abandoned when a worker eventually reaches it. The worker's write to
		// Done cannot block because Done is buffered.
		return Result{Err: ctx.Err()}
	}
}

// gateAdmitFraction is the occupancy fraction at which a gated node starts
// shedding Normal-priority traffic. gateResumeFraction is where it stops —
// deliberately well below gateAdmitFraction, and specifically matched to
// gateDegradedFraction (rollup.go) so admission only reopens once the
// backlog is back down to merely-DEGRADED territory, not just under the shed
// line. RC calls skip this check entirely and are only ever shed if the
// queue is genuinely at its hard capacity. This is deliberately not a
// separate occupancy counter — the resource's own buffered channel already
// tracks depth (queueDepth on NodeSnapshot), so gating just adds a
// priority-aware, debounced admission threshold in front of the existing
// enqueue rather than building parallel state that could drift from it.
//
// A hysteresis band this wide is still not enough to make the gate settle
// under *sustained* overload — with admission fully open, arrival can still
// outrun capacity, so a binary on/off gate is structurally a limit cycle
// (fills to gateAdmitFraction, sheds down to gateResumeFraction, repeat),
// not a stable equilibrium; hysteresis only controls the cycle's period, not
// whether one exists. The wide band here is chosen so that period is long
// enough (tens of seconds) not to read as flicker in a normal demo
// interaction. A gate that converges to a true steady state under sustained
// overload would need graduated admission (AIMD) instead of on/off — noted
// under "what I'd do with more time."
const (
	gateAdmitFraction  = 0.9
	gateResumeFraction = gateDegradedFraction
)

// gatedCall is the entry point for a ModeGated dependency: the resource being
// gated owns the backlog, not the caller. A short synchronous grace window
// keeps baseline behaviour (the resource is healthy) identical to a normal
// call; if it elapses, the call is promoted to a detached background attempt
// instead of being abandoned, and the caller is freed immediately.
//
// Promotion is why the job is enqueued with its own long-lived context
// (bounded by gateHoldBudget) rather than the caller's ctx: the caller's run
// deadline is about to expire regardless, and detaching is what lets the
// same logical attempt keep waiting for real capacity instead of being
// thrown away and silently retried from scratch.
func (s *service) gatedCall(ctx context.Context, priority Priority) Result {
	if priority == PriorityNormal && s.admitCheck() {
		s.metrics.RecordRejected()
		return Result{Err: ErrQueueFull}
	}

	holdCtx, cancel := context.WithTimeout(context.Background(), s.gateHoldBudget)
	j := &job{Ctx: holdCtx, EnqueuedAt: time.Now(), Done: make(chan Result, 1)}

	select {
	case s.queue <- j:
	default:
		cancel()
		s.metrics.RecordRejected()
		return Result{Err: ErrQueueFull}
	}

	grace := time.NewTimer(nonEssentialTimeout)
	defer grace.Stop()
	select {
	case res := <-j.Done:
		cancel()
		return res
	case <-grace.C:
	case <-ctx.Done():
		// The caller gave up before the grace window even elapsed. Treat it
		// the same as the grace window elapsing: the job itself still isn't
		// cancelled, because holdCtx was never derived from ctx.
	}

	go func() {
		defer cancel()
		<-j.Done
	}()
	return Result{Degraded: true}
}

// worker drains the queue until it is closed.
func (s *service) worker() {
	defer s.wg.Done()
	for j := range s.queue {
		// Drop work whose caller already gave up. Doing the work anyway would
		// burn capacity the still-live callers need, which is how an overloaded
		// system stays overloaded long after the burst passed.
		if err := j.Ctx.Err(); err != nil {
			s.metrics.RecordAbandoned()
			j.Done <- Result{Err: err}
			continue
		}

		wait := time.Since(j.EnqueuedAt)
		s.metrics.RecordQueueWait(wait)

		s.inFlight.Add(1)
		res := s.process(j.Ctx)
		s.inFlight.Add(-1)

		s.metrics.RecordCompleted(time.Since(j.EnqueuedAt), wait, res.Err != nil, res.Degraded)
		j.Done <- res
	}
}

// process performs this node's own work, then fans out to its dependencies.
//
// The occupancy this function takes is the physics of the whole model: it is
// what determines whether the caller's queue drains faster than it fills.
//   - Own compute: baseLatency * injected multiplier * jitter.
//   - ModeBlocking children are called with the caller's full remaining context
//     and blocked on, so their latency is fully additive to this node's
//     occupancy. That is the mechanism by which a slow leaf backs up its
//     parents.
//   - ModeBestEffort children are called with a 300ms budget and their outcome
//     is downgraded to a Degraded flag. The cost is bounded, so the parent's
//     occupancy barely moves and its queue stays flat. That containment is not
//     asserted anywhere; it falls out of the timeout.
//   - ModeGated children go through gatedCall instead: essential for
//     propagation purposes (see modeEssential), but dispatched through the
//     grace-window/hold/shed mechanism rather than a direct blocking call —
//     see gatedCall's doc comment.
func (s *service) process(ctx context.Context) Result {
	mult := s.latencyMultiplier.Load()
	if !(mult > 0) { // also catches NaN
		mult = 1
	}
	// +/-15% jitter so latencies form a distribution rather than a spike.
	jitter := 0.85 + 0.30*rand.Float64()
	work := time.Duration(float64(s.baseLatency) * mult * jitter)

	if work > 0 {
		timer := time.NewTimer(work)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Result{Err: ctx.Err()}
		}
	}

	if fr := s.failRate.Load(); fr > 0 && rand.Float64() < fr {
		return Result{Err: ErrInjected}
	}

	if len(s.deps) == 0 {
		return Result{}
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		res Result
	)
	priority := priorityFromContext(ctx)
	for _, dep := range s.deps {
		// Read the classification exactly once per call. Reading it again on the
		// result path would let a mid-flight toggle take one branch on the way
		// down and the other on the way back up.
		mode := dep.mode.Load()

		wg.Add(1)
		go func(dep *dependency, mode DependencyMode) {
			defer wg.Done()

			var r Result
			switch mode {
			case ModeGated:
				r = dep.child.gatedCall(ctx, priority)
			case ModeBlocking:
				r = dep.child.call(ctx)
			default: // ModeBestEffort
				cctx, cancel := context.WithTimeout(ctx, dep.timeout)
				r = dep.child.call(cctx)
				cancel()
			}

			mu.Lock()
			defer mu.Unlock()
			if modeEssential(mode) {
				if r.Err != nil && res.Err == nil {
					res.Err = r.Err
				}
				if r.Degraded {
					res.Degraded = true
				}
				return
			}
			// Best-effort: an error or a timeout is information, not a failure.
			if r.Err != nil || r.Degraded {
				res.Degraded = true
			}
		}(dep, mode)
	}
	wg.Wait()
	return res
}

// queueDepth is the live occupancy of the queue channel.
func (s *service) queueDepth() int { return len(s.queue) }

// queueCapacity is the channel's buffer size.
func (s *service) queueCapacity() int { return cap(s.queue) }

// admitCheck updates and reads the shedding latch: crosses to true once
// occupancy reaches gateAdmitFraction, back to false only once it drains to
// gateResumeFraction, holding steady in between. Returns true when a
// Normal-priority call should be shed.
func (s *service) admitCheck() bool {
	occupied := float64(s.queueDepth())
	capacity := float64(s.queueCapacity())
	if capacity <= 0 {
		return false
	}
	frac := occupied / capacity
	switch {
	case frac >= gateAdmitFraction:
		s.shedding.Store(true)
	case frac <= gateResumeFraction:
		s.shedding.Store(false)
	}
	return s.shedding.Load()
}

// isShedding reports the current value of the hysteresis latch without
// mutating it — used by tick() to key a gate's FAILING status to the exact
// same debounced signal that drives real admission, so the two can never
// read differently at the same instant.
func (s *service) isShedding() bool { return s.shedding.Load() }
