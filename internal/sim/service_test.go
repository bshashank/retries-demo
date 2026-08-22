package sim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestEnqueueShedsLoadInsteadOfBlocking is the load-shedding contract: when the
// queue is full a caller is refused immediately. If this ever blocked, a slow
// leaf would silently stall its callers instead of showing up as a reject rate.
func TestEnqueueShedsLoadInsteadOfBlocking(t *testing.T) {
	t.Parallel()

	// One worker, capacity 2, and work slow enough that nothing drains during
	// the test.
	s := newService(nodeSpec{
		ID: "x", Label: "X", Tier: 1,
		BaseLatency: 2 * time.Second, Workers: 1, QueueCap: 2,
	})
	s.start()
	defer s.shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Fill the pipeline: one job in a worker, two more parked in the buffer.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.call(ctx)
		}()
	}

	// Wait for the queue to actually be saturated rather than guessing.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.queueDepth() == s.queueCapacity() && s.inFlight.Load() == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if s.queueDepth() != s.queueCapacity() {
		t.Fatalf("queue depth = %d, want it saturated at %d", s.queueDepth(), s.queueCapacity())
	}

	// The shed must be immediate, not merely eventual.
	start := time.Now()
	res := s.call(ctx)
	elapsed := time.Since(start)

	if !errors.Is(res.Err, ErrQueueFull) {
		t.Fatalf("call on a full queue returned %v, want ErrQueueFull", res.Err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("enqueue blocked for %v on a full queue; it must shed immediately", elapsed)
	}

	v := s.metrics.Read(time.Now())
	if v.Rejected != 1 {
		t.Fatalf("rejected count = %d, want 1", v.Rejected)
	}
	if v.RejectRate <= 0 {
		t.Fatalf("RejectRate = %v, want > 0", v.RejectRate)
	}

	cancel()
	wg.Wait()
}

// TestCancelledJobIsAbandonedNotProcessed covers the other half of the worker
// loop: work whose caller already gave up is dropped at dequeue. Doing it anyway
// would burn capacity that live callers need.
func TestCancelledJobIsAbandonedNotProcessed(t *testing.T) {
	t.Parallel()

	s := newService(nodeSpec{
		ID: "x", Label: "X", Tier: 1,
		BaseLatency: 500 * time.Millisecond, Workers: 1, QueueCap: 8,
	})

	// Enqueue by hand, before any worker exists, with a context that is already
	// dead.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	j := &job{Ctx: dead, EnqueuedAt: time.Now(), Done: make(chan Result, 1)}
	s.queue <- j

	s.start()
	defer s.shutdown()

	select {
	case res := <-j.Done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("result err = %v, want context.Canceled", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker never returned a result for the abandoned job")
	}

	// It must have been abandoned in well under the 500ms it would have taken to
	// actually process, and recorded as abandoned rather than completed.
	v := s.metrics.Read(time.Now())
	if v.Abandoned != 1 {
		t.Fatalf("abandoned = %d, want 1", v.Abandoned)
	}
	if v.Completed != 0 {
		t.Fatalf("completed = %d, want 0: the job must not have been processed", v.Completed)
	}
	if v.AbandonRate != 1 {
		t.Fatalf("AbandonRate = %v, want 1", v.AbandonRate)
	}
}

// A worker must never park writing a result to a caller that has already timed
// out. Done is buffered with capacity 1 precisely so this cannot happen; if that
// regressed, the worker would leak and the pool would bleed capacity.
func TestWorkerNeverBlocksOnAbandonedCaller(t *testing.T) {
	t.Parallel()

	s := newService(nodeSpec{
		ID: "x", Label: "X", Tier: 1,
		BaseLatency: 30 * time.Millisecond, Workers: 1, QueueCap: 4,
	})
	s.start()
	defer s.shutdown()

	// Every one of these callers gives up long before the work finishes.
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		res := s.call(ctx)
		cancel()
		if res.Err == nil {
			t.Fatalf("call %d unexpectedly succeeded before its deadline", i)
		}
	}

	// The single worker must still be able to serve a fresh caller: if it were
	// parked writing to an abandoned Done channel it would never get here.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if res := s.call(ctx); res.Err != nil {
		t.Fatalf("worker pool wedged after abandoned callers: %v", res.Err)
	}
}

func TestProcessRespectsCallerDeadline(t *testing.T) {
	t.Parallel()

	s := newService(nodeSpec{
		ID: "slow", Label: "Slow", Tier: 1,
		BaseLatency: 5 * time.Second, Workers: 2, QueueCap: 8,
	})
	s.start()
	defer s.shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := s.call(ctx)
	elapsed := time.Since(start)

	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", res.Err)
	}
	if elapsed > time.Second {
		t.Fatalf("call took %v; own compute must abort on ctx.Done, not run to completion", elapsed)
	}
}

func TestInjectedFailureRate(t *testing.T) {
	t.Parallel()

	s := newService(nodeSpec{
		ID: "x", Label: "X", Tier: 1,
		BaseLatency: time.Millisecond, Workers: 4, QueueCap: 64,
	})
	s.failRate.Store(1.0)
	s.start()
	defer s.shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		if res := s.call(ctx); !errors.Is(res.Err, ErrInjected) {
			t.Fatalf("call %d err = %v, want ErrInjected at failRate 1.0", i, res.Err)
		}
	}
}

// The two dependency kinds must cost the caller very different amounts of
// occupancy: that difference is the entire containment mechanism.
func TestNonEssentialDependencyIsBoundedAndNonFatal(t *testing.T) {
	t.Parallel()

	parent := newService(nodeSpec{
		ID: "parent", Label: "Parent", Tier: 1,
		BaseLatency: time.Millisecond, Workers: 4, QueueCap: 32,
	})
	// A child far slower than the 300ms budget.
	child := newService(nodeSpec{
		ID: "child", Label: "Child", Tier: 2,
		BaseLatency: 3 * time.Second, Workers: 4, QueueCap: 32,
	})

	dep := &dependency{from: "parent", to: "child", child: child, timeout: nonEssentialTimeout}
	dep.essential.Store(false)
	parent.deps = append(parent.deps, dep)

	parent.start()
	child.start()
	defer func() { parent.shutdown(); child.shutdown() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	res := parent.call(ctx)
	elapsed := time.Since(start)

	if res.Err != nil {
		t.Fatalf("non-essential failure errored the parent: %v (it must only degrade)", res.Err)
	}
	if !res.Degraded {
		t.Fatal("parent should be flagged Degraded by a failed non-essential dependency")
	}
	// Bounded: the parent paid the 300ms budget, not the child's 3s.
	if elapsed > time.Second {
		t.Fatalf("parent occupancy was %v; a non-essential dep must cost at most ~%v", elapsed, nonEssentialTimeout)
	}
	if elapsed < nonEssentialTimeout {
		t.Fatalf("parent returned in %v, before the %v budget elapsed", elapsed, nonEssentialTimeout)
	}
}

func TestEssentialDependencyIsAdditiveAndFatal(t *testing.T) {
	t.Parallel()

	parent := newService(nodeSpec{
		ID: "parent", Label: "Parent", Tier: 1,
		BaseLatency: 20 * time.Millisecond, Workers: 4, QueueCap: 32,
	})
	child := newService(nodeSpec{
		ID: "child", Label: "Child", Tier: 2,
		BaseLatency: 400 * time.Millisecond, Workers: 4, QueueCap: 32,
	})

	dep := &dependency{from: "parent", to: "child", child: child, timeout: nonEssentialTimeout}
	dep.essential.Store(true)
	parent.deps = append(parent.deps, dep)

	parent.start()
	child.start()
	defer func() { parent.shutdown(); child.shutdown() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	res := parent.call(ctx)
	elapsed := time.Since(start)

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Fully additive: the parent blocked on the child rather than timing it out
	// at 300ms.
	if elapsed < 380*time.Millisecond {
		t.Fatalf("parent occupancy %v is less than the child's own latency; essential calls must be blocking and additive", elapsed)
	}

	// Now make the child fail and confirm hard propagation.
	child.failRate.Store(1.0)
	if res := parent.call(ctx); !errors.Is(res.Err, ErrInjected) {
		t.Fatalf("parent err = %v, want the essential child's error propagated", res.Err)
	}
}

func TestEssentialToggleIsReadOncePerCall(t *testing.T) {
	t.Parallel()

	parent := newService(nodeSpec{
		ID: "parent", Label: "Parent", Tier: 1,
		BaseLatency: time.Millisecond, Workers: 8, QueueCap: 64,
	})
	child := newService(nodeSpec{
		ID: "child", Label: "Child", Tier: 2,
		BaseLatency: 5 * time.Millisecond, Workers: 8, QueueCap: 64,
	})
	dep := &dependency{from: "parent", to: "child", child: child, timeout: nonEssentialTimeout}
	dep.essential.Store(false)
	parent.deps = append(parent.deps, dep)
	child.failRate.Store(1.0)

	parent.start()
	child.start()
	defer func() { parent.shutdown(); child.shutdown() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Hammer the parent while flipping the classification underneath it. Every
	// individual result must be internally consistent: either "non-essential, so
	// degraded but not errored", or "essential, so errored".
	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		v := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			v = !v
			dep.essential.Store(v)
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				res := parent.call(ctx)
				if res.Err != nil && res.Degraded {
					// A single call cannot have taken both branches for the same
					// dependency.
					t.Errorf("result took both branches: err=%v degraded=%v", res.Err, res.Degraded)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	flipper.Wait()
}

func TestServiceQueueAccessors(t *testing.T) {
	t.Parallel()
	s := newService(nodeSpec{ID: "x", Label: "X", Tier: 1, BaseLatency: time.Millisecond, Workers: 2, QueueCap: 7})
	if s.queueCapacity() != 7 {
		t.Fatalf("queueCapacity = %d, want 7", s.queueCapacity())
	}
	if s.queueDepth() != 0 {
		t.Fatalf("queueDepth = %d, want 0", s.queueDepth())
	}
}

func TestAtomicFloat(t *testing.T) {
	t.Parallel()
	var a atomicFloat
	if got := a.Load(); got != 0 {
		t.Fatalf("zero value = %v, want 0", got)
	}
	a.Store(12.5)
	if got := a.Load(); got != 12.5 {
		t.Fatalf("Load = %v, want 12.5", got)
	}
}
