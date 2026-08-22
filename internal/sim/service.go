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

// dependency is one outgoing edge. Essential is an atomic.Bool because operators
// can reclassify an edge while calls are in flight.
type dependency struct {
	from  string
	to    string
	child *service

	essential        atomic.Bool
	defaultEssential bool
	timeout          time.Duration
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

	latencyMultiplier atomicFloat
	failRate          atomicFloat
	inFlight          atomic.Int64

	metrics *metrics
	wg      sync.WaitGroup
}

func newService(spec nodeSpec) *service {
	s := &service{
		id:          spec.ID,
		label:       spec.Label,
		tier:        spec.Tier,
		baseLatency: spec.BaseLatency,
		workerCount: spec.Workers,
		queue:       make(chan *job, spec.QueueCap),
		metrics:     newMetrics(metricsWindow),
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
//   - Essential children are called with the caller's full remaining context and
//     blocked on, so their latency is fully additive to this node's occupancy.
//     That is the mechanism by which a slow leaf backs up its parents.
//   - Non-essential children are called with a 300ms budget and their outcome is
//     downgraded to a Degraded flag. The cost is bounded, so the parent's
//     occupancy barely moves and its queue stays flat. That containment is not
//     asserted anywhere; it falls out of the timeout.
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
	for _, dep := range s.deps {
		// Read the classification exactly once per call. Reading it again on the
		// result path would let a mid-flight toggle take one branch on the way
		// down and the other on the way back up.
		essential := dep.essential.Load()

		wg.Add(1)
		go func(dep *dependency, essential bool) {
			defer wg.Done()

			var r Result
			if essential {
				r = dep.child.call(ctx)
			} else {
				cctx, cancel := context.WithTimeout(ctx, dep.timeout)
				r = dep.child.call(cctx)
				cancel()
			}

			mu.Lock()
			defer mu.Unlock()
			if essential {
				if r.Err != nil && res.Err == nil {
					res.Err = r.Err
				}
				if r.Degraded {
					res.Degraded = true
				}
				return
			}
			// Non-essential: an error or a timeout is information, not a failure.
			if r.Err != nil || r.Degraded {
				res.Degraded = true
			}
		}(dep, essential)
	}
	wg.Wait()
	return res
}

// queueDepth is the live occupancy of the queue channel.
func (s *service) queueDepth() int { return len(s.queue) }

// queueCapacity is the channel's buffer size.
func (s *service) queueCapacity() int { return cap(s.queue) }
