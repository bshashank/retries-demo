package sim

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes. Polling the engine
// beats sleeping a fixed amount: the simulation is a real concurrent system and
// its settling time varies with machine load and the race detector.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func(Snapshot) bool, snap func() Snapshot) Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Snapshot
	for time.Now().Before(deadline) {
		last = snap()
		if cond(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s; last global=%s nodes=%s", timeout, what, last.Global, describe(last))
	return last
}

func describe(s Snapshot) string {
	out := ""
	for _, n := range s.Nodes {
		out += "\n    " + n.ID + " local=" + string(n.LocalStatus) + " rollup=" + string(n.RollupStatus) +
			" q=" + itoa(n.QueueDepth) + "/" + itoa(n.QueueCapacity) +
			" wait=" + ftoa(n.MeanQueueWaitMs) + "ms p95=" + ftoa(n.P95LatencyMs) + "ms" +
			" err=" + ftoa(n.ErrorRate) + " rej=" + ftoa(n.RejectRate) + " aband=" + ftoa(n.AbandonRate)
	}
	return out
}

func nodeByID(s Snapshot, id string) NodeSnapshot {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n
		}
	}
	return NodeSnapshot{}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func ftoa(v float64) string {
	whole := int(v)
	frac := int((v - float64(whole)) * 100)
	if frac < 0 {
		frac = -frac
	}
	s := itoa(whole) + "."
	if frac < 10 {
		s += "0"
	}
	return s + itoa(frac)
}

func TestEngineSnapshotIsUsableImmediately(t *testing.T) {
	e := New()
	defer e.Close()

	// New must publish before it returns; the API layer can serve a snapshot on
	// its very first request.
	s := e.Snapshot()
	if len(s.Nodes) != 9 || len(s.Edges) != 9 {
		t.Fatalf("initial snapshot not populated: %d nodes, %d edges", len(s.Nodes), len(s.Edges))
	}
	if s.Global != StatusOK {
		t.Fatalf("initial global = %s, want OK", s.Global)
	}
	if s.AtMs == 0 {
		t.Fatal("initial snapshot has no timestamp")
	}
	if len(s.Events) == 0 {
		t.Fatal("expected a startup event")
	}
}

func TestEngineBaselineIsHealthy(t *testing.T) {
	if testing.Short() {
		t.Skip("baseline settle needs ~8s")
	}
	e := New()
	defer e.Close()

	// Let the 5s window fill.
	time.Sleep(7 * time.Second)
	s := e.Snapshot()

	if s.Global != StatusOK {
		t.Fatalf("baseline global = %s, want OK.%s", s.Global, describe(s))
	}
	for _, n := range s.Nodes {
		if n.LocalStatus != StatusOK || n.RollupStatus != StatusOK {
			t.Errorf("baseline node %s local=%s rollup=%s, want OK/OK", n.ID, n.LocalStatus, n.RollupStatus)
		}
		if n.RejectRate > 0 {
			t.Errorf("baseline node %s is shedding load (rejectRate=%v)", n.ID, n.RejectRate)
		}
		if n.QueueDepth > n.QueueCapacity/4 {
			t.Errorf("baseline node %s queue depth %d is not near empty", n.ID, n.QueueDepth)
		}
	}

	orch := nodeByID(s, NodeOrchestrator)
	if orch.P95LatencyMs > 1000 {
		t.Errorf("baseline orchestrator p95 = %vms, want well under the 2000ms run deadline", orch.P95LatencyMs)
	}
	if s.RunsPerSec < 12 || s.RunsPerSec > 30 {
		t.Errorf("runsPerSec = %v, want roughly 20", s.RunsPerSec)
	}
	if s.RunSuccessRate < 0.98 {
		t.Errorf("baseline run success rate = %v, want ~1.0", s.RunSuccessRate)
	}
}

// TestNonEssentialFailureIsContained is the headline behaviour: a 10x stall on
// the SAST engine takes its own subtree red but is capped at DEGRADED globally,
// and - the part that proves it is real physics and not a display rule - the
// orchestrator's own queue never moves.
func TestNonEssentialFailureIsContained(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario needs ~25s to settle and hold")
	}
	e := New()
	defer e.Close()

	time.Sleep(6 * time.Second) // fill the window at baseline
	base := e.Snapshot()
	baseOrch := nodeByID(base, NodeOrchestrator)

	if err := e.ApplyScenario(ScenarioSASTSlow); err != nil {
		t.Fatalf("ApplyScenario: %v", err)
	}

	got := waitFor(t, 30*time.Second, "global to reach DEGRADED under sast-slow",
		func(s Snapshot) bool { return s.Global == StatusDegraded }, e.Snapshot)

	if sast := nodeByID(got, NodeSAST); sast.RollupStatus == StatusOK {
		t.Errorf("sast-engine rollup = %s, want it visibly unhealthy", sast.RollupStatus)
	}

	// Hold for a while: DEGRADED must be a stable equilibrium, never FAILING.
	deadline := time.Now().Add(15 * time.Second)
	sawSecurityScanRed := false
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if s.Global == StatusFailing {
			t.Fatalf("global reached FAILING under a non-essential outage; the cap leaked.%s", describe(s))
		}
		if nodeByID(s, NodeSecurityScan).RollupStatus == StatusFailing {
			sawSecurityScanRed = true
		}

		orch := nodeByID(s, NodeOrchestrator)
		// The containment proof: the orchestrator absorbs the stall in its
		// worker pool, not in its queue.
		if orch.QueueDepth > 16 {
			t.Fatalf("orchestrator queue depth reached %d; a non-essential stall must not back it up.%s",
				orch.QueueDepth, describe(s))
		}
		if orch.MeanQueueWaitMs > degradedQueueWaitMs {
			t.Fatalf("orchestrator mean queue wait reached %vms (baseline %vms); a non-essential stall must not back it up.%s",
				orch.MeanQueueWaitMs, baseOrch.MeanQueueWaitMs, describe(s))
		}
		if orch.RejectRate > 0 {
			t.Fatalf("orchestrator is shedding load (%v) under a non-essential outage.%s", orch.RejectRate, describe(s))
		}
		if orch.LocalStatus == StatusFailing {
			t.Fatalf("orchestrator local status FAILING under a non-essential outage.%s", describe(s))
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !sawSecurityScanRed {
		t.Error("security-scan never went FAILING; its essential SAST dependency should have taken it red")
	}

	final := e.Snapshot()
	if final.Global != StatusDegraded {
		t.Fatalf("global settled at %s, want a stable DEGRADED.%s", final.Global, describe(final))
	}
	// Runs still complete: a non-essential outage costs latency, not success.
	if final.RunSuccessRate < 0.9 {
		t.Errorf("run success rate = %v under sast-slow, want runs to still succeed", final.RunSuccessRate)
	}
}

// TestEssentialFailureEscalates is the contrast case: the same class of stall on
// a shared essential leaf does reach FAILING.
func TestEssentialFailureEscalates(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario needs ~30s")
	}
	e := New()
	defer e.Close()

	time.Sleep(4 * time.Second)
	if err := e.ApplyScenario(ScenarioArtifactOutage); err != nil {
		t.Fatalf("ApplyScenario: %v", err)
	}

	start := time.Now()
	got := waitFor(t, 40*time.Second, "artifact-store and global to reach FAILING under artifact-outage",
		func(s Snapshot) bool {
			return s.Global == StatusFailing && nodeByID(s, NodeArtifactStore).RollupStatus == StatusFailing
		}, e.Snapshot)
	t.Logf("global and artifact-store reached FAILING %v after injection", time.Since(start).Round(100*time.Millisecond))

	// Both essential parents of the shared leaf must have backed up.
	for _, id := range []string{NodeArtifactStore, NodeBuild, NodeTest} {
		if n := nodeByID(got, id); n.RollupStatus != StatusFailing {
			t.Errorf("%s rollup = %s, want FAILING.%s", id, n.RollupStatus, describe(got))
		}
	}
	// And the pressure must be visible as real queueing, not just a status flip.
	store := nodeByID(got, NodeArtifactStore)
	if store.QueueDepth == 0 && store.MeanQueueWaitMs < 100 && store.AbandonRate == 0 {
		t.Errorf("artifact-store shows no queueing pressure: %+v", store)
	}
}

// The two scenarios differ only in which edge sits above them; that difference
// alone decides DEGRADED versus FAILING.
func TestScenarioContrastAndRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("needs ~40s")
	}
	e := New()
	defer e.Close()

	time.Sleep(4 * time.Second)

	if err := e.ApplyScenario(ScenarioArtifactOutage); err != nil {
		t.Fatalf("ApplyScenario: %v", err)
	}
	waitFor(t, 40*time.Second, "artifact-outage to reach FAILING",
		func(s Snapshot) bool { return s.Global == StatusFailing }, e.Snapshot)

	// Reset must recover naturally rather than by fiat.
	e.Reset()
	rec := waitFor(t, 40*time.Second, "recovery to OK after reset",
		func(s Snapshot) bool { return s.Global == StatusOK }, e.Snapshot)
	for _, n := range rec.Nodes {
		if n.LatencyMultiplier != 1 || n.FailRate != 0 {
			t.Errorf("node %s still injected after reset: mult=%v fail=%v", n.ID, n.LatencyMultiplier, n.FailRate)
		}
	}
}

func TestKafkaLagGrowsQueueAndStaysDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario needs ~25s")
	}
	e := New()
	defer e.Close()

	time.Sleep(4 * time.Second)
	if err := e.ApplyScenario(ScenarioKafkaLag); err != nil {
		t.Fatalf("ApplyScenario: %v", err)
	}

	got := waitFor(t, 30*time.Second, "kafka-lag to degrade global",
		func(s Snapshot) bool { return s.Global == StatusDegraded }, e.Snapshot)

	kafka := nodeByID(got, NodeKafka)
	if kafka.QueueDepth == 0 && kafka.MeanQueueWaitMs < 100 {
		t.Errorf("kafka-bus shows no queue growth: depth=%d wait=%vms", kafka.QueueDepth, kafka.MeanQueueWaitMs)
	}
	if kafka.LocalStatus == StatusOK {
		t.Errorf("kafka-bus local = OK, want it degraded by its own queue")
	}

	// Telemetry hangs off a non-essential edge, so this must stay capped.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if s.Global == StatusFailing {
			t.Fatalf("kafka-lag reached FAILING; telemetry is non-essential.%s", describe(s))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Reclassifying the orchestrator's security-scan edge as essential converts the
// contained failure into a fatal one, using the same injection.
func TestEdgeReclassificationChangesOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("needs ~30s")
	}
	e := New()
	defer e.Close()

	time.Sleep(4 * time.Second)
	if err := e.ApplyScenario(ScenarioSASTSlow); err != nil {
		t.Fatalf("ApplyScenario: %v", err)
	}
	waitFor(t, 30*time.Second, "sast-slow to reach DEGRADED",
		func(s Snapshot) bool { return s.Global == StatusDegraded }, e.Snapshot)

	if err := e.SetEdgeEssential(NodeOrchestrator, NodeSecurityScan, true); err != nil {
		t.Fatalf("SetEdgeEssential: %v", err)
	}
	waitFor(t, 30*time.Second, "global to escalate once the edge is essential",
		func(s Snapshot) bool { return s.Global == StatusFailing }, e.Snapshot)
}

func TestControllerErrorsOnUnknownIDs(t *testing.T) {
	e := New()
	defer e.Close()

	if err := e.Inject("no-such-node", 2, 0); err == nil {
		t.Error("Inject accepted an unknown node ID")
	}
	if err := e.Inject(NodeBuild, 2, 0.1); err != nil {
		t.Errorf("Inject on a known node failed: %v", err)
	}
	if err := e.SetEdgeEssential("no-such-node", NodeBuild, true); err == nil {
		t.Error("SetEdgeEssential accepted an unknown source")
	}
	if err := e.SetEdgeEssential(NodeBuild, "no-such-node", true); err == nil {
		t.Error("SetEdgeEssential accepted an unknown target")
	}
	// A real pair of nodes that is not actually an edge.
	if err := e.SetEdgeEssential(NodeOrchestrator, NodeSAST, true); err == nil {
		t.Error("SetEdgeEssential accepted a non-existent edge between two real nodes")
	}
	if err := e.SetEdgeEssential(NodeOrchestrator, NodeBuild, false); err != nil {
		t.Errorf("SetEdgeEssential on a real edge failed: %v", err)
	}
	if err := e.ApplyScenario("no-such-scenario"); err == nil {
		t.Error("ApplyScenario accepted an unknown name")
	}
	for _, s := range e.Scenarios() {
		if err := e.ApplyScenario(s.Name); err != nil {
			t.Errorf("advertised scenario %q is not applicable: %v", s.Name, err)
		}
	}
}

func TestInjectSanitisesInput(t *testing.T) {
	e := New()
	defer e.Close()

	if err := e.Inject(NodeBuild, -5, -1); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	n := nodeByID(waitForTick(t, e), NodeBuild)
	if n.LatencyMultiplier != 1 || n.FailRate != 0 {
		t.Fatalf("negative input not sanitised: mult=%v fail=%v", n.LatencyMultiplier, n.FailRate)
	}

	if err := e.Inject(NodeBuild, 1e9, 5); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	n = nodeByID(waitForTick(t, e), NodeBuild)
	if n.LatencyMultiplier > 1000 || n.FailRate != 1 {
		t.Fatalf("oversized input not clamped: mult=%v fail=%v", n.LatencyMultiplier, n.FailRate)
	}
	e.Reset()
}

func waitForTick(t *testing.T, e *Engine) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	before := e.Snapshot().AtMs
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if s.AtMs != before {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("engine stopped ticking")
	return Snapshot{}
}

func TestScenariosAreDocumented(t *testing.T) {
	e := New()
	defer e.Close()

	got := e.Scenarios()
	if len(got) != 4 {
		t.Fatalf("got %d scenarios, want 4", len(got))
	}
	want := map[string]bool{
		ScenarioNominal: false, ScenarioSASTSlow: false,
		ScenarioArtifactOutage: false, ScenarioKafkaLag: false,
	}
	for _, s := range got {
		if _, ok := want[s.Name]; !ok {
			t.Errorf("unexpected scenario %q", s.Name)
			continue
		}
		want[s.Name] = true
		if s.Label == "" || s.Description == "" || s.Expected == "" {
			t.Errorf("scenario %q is not fully documented: %+v", s.Name, s)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("scenario %q missing", name)
		}
	}

	// The returned slice must be a copy; a caller must not be able to corrupt
	// the catalogue.
	got[0].Label = "mutated"
	if e.Scenarios()[0].Label == "mutated" {
		t.Error("Scenarios returns the shared backing array")
	}
}

func TestEventsRingIsBoundedAndOrdered(t *testing.T) {
	e := New()
	defer e.Close()

	for i := 0; i < maxEvents*2; i++ {
		e.emit(LevelInfo, "filler")
	}
	s := waitForTick(t, e)
	if len(s.Events) > maxEvents {
		t.Fatalf("event ring holds %d events, want at most %d", len(s.Events), maxEvents)
	}
	if len(s.Events) != maxEvents {
		t.Fatalf("event ring holds %d events, want it full at %d", len(s.Events), maxEvents)
	}
	for i := 1; i < len(s.Events); i++ {
		if s.Events[i].ID <= s.Events[i-1].ID {
			t.Fatalf("events not in ascending ID order at %d: %d then %d", i, s.Events[i-1].ID, s.Events[i].ID)
		}
	}
	if s.Events[0].AtMs == 0 {
		t.Error("events carry no timestamp")
	}
}

func TestOperatorActionsEmitEvents(t *testing.T) {
	e := New()
	defer e.Close()

	before := len(e.Snapshot().Events)
	if err := e.ApplyScenario(ScenarioSASTSlow); err != nil {
		t.Fatal(err)
	}
	if err := e.SetEdgeEssential(NodeBuild, NodeRegistry, true); err != nil {
		t.Fatal(err)
	}
	e.Reset()

	s := waitForTick(t, e)
	if len(s.Events) <= before {
		t.Fatalf("operator actions produced no events (%d -> %d)", before, len(s.Events))
	}
}

// Concurrent readers must never see a torn snapshot or slow the engine down.
func TestSnapshotIsSafeForConcurrentReaders(t *testing.T) {
	e := New()
	defer e.Close()

	done := make(chan struct{})
	for i := 0; i < 32; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				s := e.Snapshot()
				if len(s.Nodes) != 9 {
					panic("torn snapshot")
				}
				for range s.Edges {
				}
				for range s.Events {
				}
			}
		}()
	}
	// Mutate engine state while they read.
	for i := 0; i < 20; i++ {
		_ = e.Inject(NodeSAST, float64(i%5+1), 0)
		_ = e.SetEdgeEssential(NodeOrchestrator, NodeTelemetry, i%2 == 0)
		time.Sleep(10 * time.Millisecond)
	}
	close(done)
	time.Sleep(50 * time.Millisecond)
}

// TestNoGoroutineLeak is a real requirement, not hygiene: every timed-out call
// in this simulation hands a worker a result nobody is listening for, and if any
// of those paths parked a goroutine the leak would compound under exactly the
// load the demo creates.
func TestNoGoroutineLeak(t *testing.T) {
	// Settle whatever earlier tests left behind.
	baseline := settledGoroutines(t, 0)

	e := New()
	// Let load actually flow so there are in-flight runs and fan-out goroutines
	// to clean up.
	time.Sleep(2 * time.Second)

	running := runtime.NumGoroutine()
	if running <= baseline {
		t.Fatalf("engine started %d goroutines over a %d baseline; it does not look like it is running", running-baseline, baseline)
	}

	e.Close()

	after := settledGoroutines(t, baseline)
	if after > baseline+2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: baseline %d, after Close %d\n%s", baseline, after, buf[:n])
	}
}

func settledGoroutines(t *testing.T, target int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	n := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if target > 0 && cur <= target+2 {
			return cur
		}
		if target == 0 && cur == n {
			return cur
		}
		n = cur
	}
	return runtime.NumGoroutine()
}

func TestCloseIsIdempotentAndStopsTheEngine(t *testing.T) {
	e := New()
	e.Close()
	e.Close() // must not panic or block
	e.Stop()

	// Snapshot must still be readable after shutdown; the API layer may race a
	// final request against shutdown.
	if s := e.Snapshot(); len(s.Nodes) != 9 {
		t.Fatalf("snapshot unusable after Close: %d nodes", len(s.Nodes))
	}
}

func TestParentContextCancellationStopsLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e, err := NewWithContext(ctx)
	if err != nil {
		t.Fatalf("NewWithContext: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	cancel()

	// Close must still drain cleanly when the parent context did the cancelling.
	done := make(chan struct{})
	go func() {
		e.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung after the parent context was cancelled")
	}
}
