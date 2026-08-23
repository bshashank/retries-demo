package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Engine satisfies the frozen Controller contract.
var _ Controller = (*Engine)(nil)

// Engine owns the running simulation: the service graph and its worker pools,
// the load generator, and the tick loop that turns raw metrics into the cached
// snapshot the API serves.
type Engine struct {
	ctx    context.Context
	cancel context.CancelFunc

	services  map[string]*service
	ordered   []*service // declaration order; stable snapshot ordering
	shutdown  []*service // topological, parents first
	evalOrder []string   // reverse topological, leaves first
	edges     []*dependency
	root      *service

	runs *runMetrics

	// snapMu guards the published snapshot. Readers are many concurrent SSE
	// streams, writers are one tick goroutine, and readers must never be able to
	// slow the simulation down.
	snapMu sync.RWMutex
	snap   Snapshot

	evMu     sync.Mutex
	events   []Event
	nextEvID uint64

	// Owned exclusively by the tick goroutine after construction.
	prevRollup map[string]Status
	prevGlobal Status

	runWG     sync.WaitGroup
	loadDone  chan struct{}
	tickDone  chan struct{}
	kickCh    chan struct{}
	closeOnce sync.Once
}

// New builds and starts an engine against context.Background. Call Close when
// finished.
func New() *Engine {
	e, err := NewWithContext(context.Background())
	if err != nil {
		// The topology is a compile-time constant of this package; a failure
		// here means the package itself is broken.
		panic(err)
	}
	return e
}

// NewEngine is an alias for New.
func NewEngine() *Engine { return New() }

// NewWithContext builds and starts an engine whose goroutines are tied to
// parent. Cancelling parent stops the simulation; Close also drains it.
func NewWithContext(parent context.Context) (*Engine, error) {
	nodes := nodeSpecs()
	edges := edgeSpecs()
	if err := validateTopology(nodes, edges); err != nil {
		return nil, err
	}
	order, err := topoOrder(nodes, edges)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	e := &Engine{
		ctx:        ctx,
		cancel:     cancel,
		services:   make(map[string]*service, len(nodes)),
		evalOrder:  reversed(order),
		runs:       newRunMetrics(metricsWindow),
		prevRollup: make(map[string]Status, len(nodes)),
		prevGlobal: StatusOK,
		loadDone:   make(chan struct{}),
		tickDone:   make(chan struct{}),
		kickCh:     make(chan struct{}, 1),
	}

	for _, spec := range nodes {
		s := newService(spec)
		e.services[spec.ID] = s
		e.ordered = append(e.ordered, s)
		e.prevRollup[spec.ID] = StatusOK
	}
	for _, id := range order {
		e.shutdown = append(e.shutdown, e.services[id])
	}
	for _, spec := range edges {
		dep := &dependency{
			from:        spec.From,
			to:          spec.To,
			child:       e.services[spec.To],
			defaultMode: spec.Mode,
			timeout:     nonEssentialTimeout,
		}
		dep.mode.Store(spec.Mode)
		parentSvc := e.services[spec.From]
		parentSvc.deps = append(parentSvc.deps, dep)
		e.edges = append(e.edges, dep)
	}
	e.root = e.services[NodeOrchestrator]

	for _, s := range e.ordered {
		s.start()
	}

	e.emit(LevelInfo, "simulation started: 9 services, 20 pipeline runs/sec")
	e.tick() // publish a usable snapshot before New returns

	go e.generateLoad()
	go e.tickLoop()

	return e, nil
}

// Close stops the load generator, drains every worker pool, and stops the tick
// loop. It is safe to call more than once.
//
// Shutdown walks the graph in topological order: a service's queue is only
// closed once every service that could send to it has already drained. Without
// that ordering a parent worker could send on a closed channel mid-shutdown.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		e.cancel()

		// No new runs, and every run goroutine has returned, so nothing can
		// enqueue at the root any more.
		<-e.loadDone
		e.runWG.Wait()

		// Parents first: draining a parent's pool retires the fan-out goroutines
		// that are the only writers to its children's queues.
		for _, s := range e.shutdown {
			s.shutdown()
		}

		<-e.tickDone
	})
}

// Stop is an alias for Close.
func (e *Engine) Stop() { e.Close() }

// generateLoad fires pipeline runs at the orchestrator with exponentially
// distributed inter-arrival times, i.e. a Poisson process. Uniform spacing would
// be a much kinder load than anything real, and would hide the burst-driven
// queueing this demo is about.
func (e *Engine) generateLoad() {
	defer close(e.loadDone)

	for {
		// -ln(U)/rate with U in (0,1]; 1-Float64() excludes zero so the log is
		// always finite.
		u := 1.0 - rand.Float64()
		gap := time.Duration(-math.Log(u) / loadRatePerSec * float64(time.Second))

		timer := time.NewTimer(gap)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		e.runWG.Add(1)
		go func() {
			defer e.runWG.Done()
			priority := pickPriority()
			runCtx, cancel := context.WithTimeout(e.ctx, runDeadline)
			runCtx = withPriority(runCtx, priority)
			defer cancel()

			start := time.Now()
			res := e.root.call(runCtx)
			e.runs.Record(time.Since(start), res.Err == nil, priority == PriorityReleaseCandidate)
		}()
	}
}

func (e *Engine) tickLoop() {
	defer close(e.tickDone)
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.tick()
		case <-e.kickCh:
			// An operator action landed; republish immediately so the UI reacts
			// to the click rather than to the next tick.
			e.tick()
		}
	}
}

// kick asks the tick loop to republish soon. Never blocks.
func (e *Engine) kick() {
	select {
	case e.kickCh <- struct{}{}:
	default:
	}
}

// tick recomputes every node's metrics, derives local statuses, folds them into
// rollups leaves-first, emits transition events, and publishes a fresh snapshot.
//
// All snapshot state is built here, on one goroutine, so prevRollup/prevGlobal
// need no locking.
func (e *Engine) tick() {
	now := time.Now()

	views := make(map[string]metricsView, len(e.ordered))
	local := make(map[string]Status, len(e.ordered))
	deps := make(map[string][]rollupDep, len(e.ordered))
	// Captured once per node so the status computed below and the QueueDepth
	// published in the snapshot always describe the exact same instant — two
	// separate live reads of a channel under concurrent load can disagree by
	// the time the second one runs.
	depths := make(map[string]int, len(e.ordered))

	for _, s := range e.ordered {
		v := s.metrics.Read(now)
		views[s.id] = v
		depth := s.queueDepth()
		depths[s.id] = depth
		if s.gateHoldBudget > 0 {
			// Gated nodes tolerate long queue waits by design, so the
			// wait-time thresholds below don't apply to them - only
			// error/reject/abandon rates do, plus backlog occupancy.
			gv := v
			gv.MeanQueueWaitMs = 0
			local[s.id] = worseStatus(localStatus(gv), gateOccupancyStatus(depth, s.queueCapacity(), s.isShedding()))
		} else {
			local[s.id] = localStatus(v)
		}

		ds := make([]rollupDep, 0, len(s.deps))
		for _, d := range s.deps {
			ds = append(ds, rollupDep{To: d.to, Essential: modeEssential(d.mode.Load())})
		}
		deps[s.id] = ds
	}

	rollups := computeRollups(e.evalOrder, local, deps)

	nodes := make([]NodeSnapshot, 0, len(e.ordered))
	for _, s := range e.ordered {
		v := views[s.id]
		nodes = append(nodes, NodeSnapshot{
			ID:                s.id,
			Label:             s.label,
			Tier:              s.tier,
			LocalStatus:       local[s.id],
			RollupStatus:      rollups[s.id],
			QueueDepth:        depths[s.id],
			QueueCapacity:     s.queueCapacity(),
			InFlight:          int(s.inFlight.Load()),
			Workers:           s.workerCount,
			ThroughputPerSec:  round2(v.ThroughputPerSec),
			ErrorRate:         round4(v.ErrorRate),
			RejectRate:        round4(v.RejectRate),
			AbandonRate:       round4(v.AbandonRate),
			MeanQueueWaitMs:   round2(v.MeanQueueWaitMs),
			P50LatencyMs:      round2(v.P50LatencyMs),
			P95LatencyMs:      round2(v.P95LatencyMs),
			BaseLatencyMs:     float64(s.baseLatency) / float64(time.Millisecond),
			LatencyMultiplier: s.latencyMultiplier.Load(),
			FailRate:          s.failRate.Load(),
		})
	}

	edges := make([]EdgeSnapshot, 0, len(e.edges))
	for _, d := range e.edges {
		edges = append(edges, EdgeSnapshot{
			From:          d.from,
			To:            d.to,
			Mode:          d.mode.Load(),
			SupportsGated: e.services[d.to].gateHoldBudget > 0,
			TimeoutMs:     float64(d.timeout) / float64(time.Millisecond),
		})
	}

	// Per-node transitions.
	for _, s := range e.ordered {
		cur := rollups[s.id]
		if prev, ok := e.prevRollup[s.id]; !ok || prev != cur {
			if ok {
				e.emit(levelFor(cur), fmt.Sprintf("%s: %s -> %s", s.label, prev, cur))
			}
			e.prevRollup[s.id] = cur
		}
	}

	// The headline transition gets its own, distinctly worded event.
	global := rollups[NodeOrchestrator]
	if global != e.prevGlobal {
		e.emit(levelFor(global), fmt.Sprintf("PIPELINE HEALTH: %s -> %s", e.prevGlobal, global))
		e.prevGlobal = global
	}

	rv := e.runs.Read(now)

	snap := Snapshot{
		AtMs:                 now.UnixMilli(),
		Global:               global,
		RunsPerSec:           round2(rv.RunsPerSec),
		RunSuccessRate:       round4(rv.SuccessRate),
		RunP95Ms:             round2(rv.P95Ms),
		RunSuccessRateRC:     round4(rv.SuccessRateRC),
		RunSuccessRateNormal: round4(rv.SuccessRateNormal),
		Nodes:                nodes,
		Edges:                edges,
		Events:               e.eventsCopy(),
	}

	e.snapMu.Lock()
	e.snap = snap
	e.snapMu.Unlock()
}

// Snapshot returns the most recently published state. It is a cached value, so
// it stays cheap under many concurrent SSE readers; the slices it carries are
// rebuilt every tick and never mutated after publication, so callers may read
// them freely but must not write to them.
func (e *Engine) Snapshot() Snapshot {
	e.snapMu.RLock()
	defer e.snapMu.RUnlock()
	return e.snap
}

// Inject sets a latency multiplier and injected failure probability on a node.
func (e *Engine) Inject(nodeID string, latencyMultiplier, failRate float64) error {
	s, ok := e.services[nodeID]
	if !ok {
		return fmt.Errorf("sim: unknown node %q", nodeID)
	}
	mult := sanitizeMultiplier(latencyMultiplier)
	fail := sanitizeRate(failRate)

	s.latencyMultiplier.Store(mult)
	s.failRate.Store(fail)

	level := LevelInfo
	if mult > 1 || fail > 0 {
		level = LevelWarn
	}
	e.emit(level, fmt.Sprintf("injection on %s: latency x%.2f, failure rate %.0f%%", s.label, mult, fail*100))
	e.kick()
	return nil
}

// SetEdgeMode reclassifies a dependency at runtime. The change is picked up
// by the next call into the parent; calls already in flight keep the
// classification they started with. ModeGated is rejected for an edge whose
// target has no gate config, for the same reason validateTopology rejects it
// statically.
func (e *Engine) SetEdgeMode(from, to string, mode DependencyMode) error {
	if mode != ModeBlocking && mode != ModeBestEffort && mode != ModeGated {
		return fmt.Errorf("sim: unknown mode %q", mode)
	}
	for _, d := range e.edges {
		if d.from != from || d.to != to {
			continue
		}
		if mode == ModeGated && e.services[d.to].gateHoldBudget <= 0 {
			return fmt.Errorf("sim: edge %s -> %s cannot be ModeGated: %s has no gate config", from, to, to)
		}
		if d.mode.Load() == mode {
			return nil
		}
		d.mode.Store(mode)
		e.emit(LevelInfo, fmt.Sprintf("edge %s -> %s reclassified as %s", from, to, mode))
		e.kick()
		return nil
	}
	return fmt.Errorf("sim: unknown edge %q -> %q", from, to)
}

// Reset clears injections and restores default edge classifications.
//
// It deliberately does not drain queues or wipe the metrics window: recovery
// takes a few seconds while the backlog clears and stale samples age out, which
// is what recovery actually looks like.
func (e *Engine) Reset() {
	e.reset()
	e.emit(LevelInfo, "reset: injections cleared, edge classifications restored (queues drain naturally)")
	e.kick()
}

func (e *Engine) reset() {
	for _, s := range e.ordered {
		s.latencyMultiplier.Store(1)
		s.failRate.Store(0)
		s.shedding.Store(false)
	}
	for _, d := range e.edges {
		d.mode.Store(d.defaultMode)
	}
}

func (e *Engine) emit(level EventLevel, msg string) {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	e.nextEvID++
	e.events = append(e.events, Event{
		ID:      e.nextEvID,
		AtMs:    time.Now().UnixMilli(),
		Level:   level,
		Message: msg,
	})
	if len(e.events) > maxEvents {
		e.events = append(e.events[:0], e.events[len(e.events)-maxEvents:]...)
	}
}

// eventsCopy returns the ring oldest-first.
func (e *Engine) eventsCopy() []Event {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	out := make([]Event, len(e.events))
	copy(out, e.events)
	return out
}

func sanitizeMultiplier(v float64) float64 {
	if math.IsNaN(v) || v <= 0 {
		return 1
	}
	if math.IsInf(v, 1) || v > 1000 {
		return 1000
	}
	return v
}

func sanitizeRate(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}
