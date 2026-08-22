package sim

import "testing"

func TestRollupOne(t *testing.T) {
	t.Parallel()

	ess := func(s Status) depStatus { return depStatus{Status: s, Essential: true} }
	non := func(s Status) depStatus { return depStatus{Status: s, Essential: false} }

	tests := []struct {
		name  string
		local Status
		deps  []depStatus
		want  Status
	}{
		{"no deps, local OK", StatusOK, nil, StatusOK},
		{"no deps, local DEGRADED", StatusDegraded, nil, StatusDegraded},
		{"no deps, local FAILING", StatusFailing, nil, StatusFailing},

		// Local FAILING dominates everything below it.
		{"local FAILING dominates healthy deps", StatusFailing, []depStatus{ess(StatusOK), non(StatusOK)}, StatusFailing},
		{"local FAILING dominates non-essential FAILING", StatusFailing, []depStatus{non(StatusFailing)}, StatusFailing},

		// Essential children propagate hard.
		{"essential FAILING -> FAILING", StatusOK, []depStatus{ess(StatusFailing)}, StatusFailing},
		{"essential FAILING beats local DEGRADED", StatusDegraded, []depStatus{ess(StatusFailing)}, StatusFailing},
		{"essential DEGRADED -> DEGRADED", StatusOK, []depStatus{ess(StatusDegraded)}, StatusDegraded},
		{"essential OK -> OK", StatusOK, []depStatus{ess(StatusOK)}, StatusOK},

		// The headline rule: a non-essential child can never take a parent red.
		{"non-essential FAILING -> DEGRADED, never FAILING", StatusOK, []depStatus{non(StatusFailing)}, StatusDegraded},
		{"non-essential DEGRADED -> DEGRADED", StatusOK, []depStatus{non(StatusDegraded)}, StatusDegraded},
		{"non-essential OK -> OK", StatusOK, []depStatus{non(StatusOK)}, StatusOK},
		{"many non-essential FAILING still only DEGRADED", StatusOK,
			[]depStatus{non(StatusFailing), non(StatusFailing), non(StatusFailing)}, StatusDegraded},

		// Mixtures.
		{"non-essential FAILING + essential OK -> DEGRADED", StatusOK,
			[]depStatus{ess(StatusOK), non(StatusFailing)}, StatusDegraded},
		{"non-essential FAILING + essential FAILING -> FAILING", StatusOK,
			[]depStatus{ess(StatusFailing), non(StatusFailing)}, StatusFailing},
		{"local DEGRADED + all deps OK -> DEGRADED", StatusDegraded,
			[]depStatus{ess(StatusOK), non(StatusOK)}, StatusDegraded},
		{"all OK -> OK", StatusOK,
			[]depStatus{ess(StatusOK), ess(StatusOK), non(StatusOK), non(StatusOK)}, StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rollupOne(tc.local, tc.deps); got != tc.want {
				t.Fatalf("rollupOne(%s, %v) = %s, want %s", tc.local, tc.deps, got, tc.want)
			}
		})
	}
}

// TestRollupOneNonEssentialCanNeverReachFailing exhaustively confirms the cap:
// whatever a non-essential subtree does, it cannot push a healthy parent past
// DEGRADED.
func TestRollupOneNonEssentialCanNeverReachFailing(t *testing.T) {
	t.Parallel()
	all := []Status{StatusOK, StatusDegraded, StatusFailing}
	for _, a := range all {
		for _, b := range all {
			deps := []depStatus{
				{Status: a, Essential: false},
				{Status: b, Essential: false},
			}
			for _, local := range []Status{StatusOK, StatusDegraded} {
				if got := rollupOne(local, deps); got == StatusFailing {
					t.Fatalf("local=%s non-essential deps=(%s,%s) reached FAILING", local, a, b)
				}
			}
		}
	}
}

// TestComputeRollupsSharedLeaf covers the real graph shape: artifact-store is a
// leaf shared by two essential parents, and both must observe the same value.
func TestComputeRollupsSharedLeaf(t *testing.T) {
	t.Parallel()

	deps := map[string][]rollupDep{
		NodeOrchestrator: {
			{To: NodeBuild, Essential: true},
			{To: NodeTest, Essential: true},
		},
		NodeBuild: {{To: NodeArtifactStore, Essential: true}},
		NodeTest:  {{To: NodeArtifactStore, Essential: true}},
	}
	evalOrder := []string{NodeArtifactStore, NodeBuild, NodeTest, NodeOrchestrator}

	local := map[string]Status{
		NodeOrchestrator:  StatusOK,
		NodeBuild:         StatusOK,
		NodeTest:          StatusOK,
		NodeArtifactStore: StatusFailing,
	}

	got := computeRollups(evalOrder, local, deps)
	for _, id := range []string{NodeArtifactStore, NodeBuild, NodeTest, NodeOrchestrator} {
		if got[id] != StatusFailing {
			t.Fatalf("%s = %s, want FAILING (shared essential leaf failing)", id, got[id])
		}
	}
}

// TestComputeRollupsContainment is the sast-slow shape: an entire failing
// subtree hanging off a non-essential edge stops at DEGRADED.
func TestComputeRollupsContainment(t *testing.T) {
	t.Parallel()

	deps := map[string][]rollupDep{
		NodeOrchestrator: {
			{To: NodeBuild, Essential: true},
			{To: NodeSecurityScan, Essential: false},
		},
		NodeSecurityScan: {{To: NodeSAST, Essential: true}},
	}
	evalOrder := []string{NodeSAST, NodeBuild, NodeSecurityScan, NodeOrchestrator}
	local := map[string]Status{
		NodeOrchestrator: StatusOK,
		NodeBuild:        StatusOK,
		NodeSecurityScan: StatusFailing,
		NodeSAST:         StatusFailing,
	}

	got := computeRollups(evalOrder, local, deps)
	if got[NodeSAST] != StatusFailing || got[NodeSecurityScan] != StatusFailing {
		t.Fatalf("subtree should be FAILING: sast=%s security-scan=%s", got[NodeSAST], got[NodeSecurityScan])
	}
	if got[NodeOrchestrator] != StatusDegraded {
		t.Fatalf("orchestrator = %s, want DEGRADED (non-essential subtree is capped)", got[NodeOrchestrator])
	}

	// Reclassify the same edge as essential and the same failure escalates.
	deps[NodeOrchestrator][1].Essential = true
	got = computeRollups(evalOrder, local, deps)
	if got[NodeOrchestrator] != StatusFailing {
		t.Fatalf("orchestrator = %s, want FAILING once the edge is essential", got[NodeOrchestrator])
	}
}

func TestComputeRollupsUsesRealTopology(t *testing.T) {
	t.Parallel()

	nodes, edges := nodeSpecs(), edgeSpecs()
	order, err := topoOrder(nodes, edges)
	if err != nil {
		t.Fatalf("topoOrder: %v", err)
	}
	evalOrder := reversed(order)

	deps := make(map[string][]rollupDep)
	for _, e := range edges {
		deps[e.From] = append(deps[e.From], rollupDep{To: e.To, Essential: e.Essential})
	}
	local := make(map[string]Status, len(nodes))
	for _, n := range nodes {
		local[n.ID] = StatusOK
	}

	// Everything healthy.
	if got := computeRollups(evalOrder, local, deps)[NodeOrchestrator]; got != StatusOK {
		t.Fatalf("baseline global = %s, want OK", got)
	}

	// kafka-bus fails: telemetry is essential to it, but telemetry hangs off a
	// non-essential edge from the orchestrator.
	local[NodeKafka] = StatusFailing
	got := computeRollups(evalOrder, local, deps)
	if got[NodeTelemetry] != StatusFailing {
		t.Fatalf("telemetry = %s, want FAILING", got[NodeTelemetry])
	}
	if got[NodeOrchestrator] != StatusDegraded {
		t.Fatalf("global = %s, want DEGRADED", got[NodeOrchestrator])
	}

	// artifact-store fails: two essential parents, escalates all the way up.
	local[NodeKafka] = StatusOK
	local[NodeArtifactStore] = StatusFailing
	got = computeRollups(evalOrder, local, deps)
	if got[NodeBuild] != StatusFailing || got[NodeTest] != StatusFailing {
		t.Fatalf("build=%s test=%s, want both FAILING", got[NodeBuild], got[NodeTest])
	}
	if got[NodeOrchestrator] != StatusFailing {
		t.Fatalf("global = %s, want FAILING", got[NodeOrchestrator])
	}
}

func TestLocalStatusThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    metricsView
		want Status
	}{
		{"no traffic is OK, not unknown", metricsView{}, StatusOK},
		{"below minAttempts reports OK", metricsView{Completed: 2, TotalAttempts: 2, ErrorRate: 1.0, MeanQueueWaitMs: 5000}, StatusOK},

		{"healthy", metricsView{TotalAttempts: 100, MeanQueueWaitMs: 3, ErrorRate: 0}, StatusOK},

		{"queue wait 250ms exactly is still OK", metricsView{TotalAttempts: 100, MeanQueueWaitMs: 250}, StatusOK},
		{"queue wait over 250ms degrades", metricsView{TotalAttempts: 100, MeanQueueWaitMs: 250.1}, StatusDegraded},
		{"queue wait 1000ms exactly is DEGRADED", metricsView{TotalAttempts: 100, MeanQueueWaitMs: 1000}, StatusDegraded},
		{"queue wait over 1000ms fails", metricsView{TotalAttempts: 100, MeanQueueWaitMs: 1000.1}, StatusFailing},

		{"reject 1% exactly is OK", metricsView{TotalAttempts: 100, RejectRate: 0.01}, StatusOK},
		{"reject over 1% degrades", metricsView{TotalAttempts: 100, RejectRate: 0.011}, StatusDegraded},
		{"reject over 20% fails", metricsView{TotalAttempts: 100, RejectRate: 0.21}, StatusFailing},

		{"error over 10% degrades", metricsView{TotalAttempts: 100, ErrorRate: 0.11}, StatusDegraded},
		{"error over 50% fails", metricsView{TotalAttempts: 100, ErrorRate: 0.51}, StatusFailing},

		{"abandon over 10% degrades", metricsView{TotalAttempts: 100, AbandonRate: 0.11}, StatusDegraded},
		{"abandon over 50% fails", metricsView{TotalAttempts: 100, AbandonRate: 0.51}, StatusFailing},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := localStatus(tc.v); got != tc.want {
				t.Fatalf("localStatus(%+v) = %s, want %s", tc.v, got, tc.want)
			}
		})
	}
}

// localStatus must never consult the injected multiplier: degradation has to be
// earned through real queueing, not read off a flag.
func TestLocalStatusIgnoresInjection(t *testing.T) {
	t.Parallel()
	// A node running at a 10x multiplier but keeping up is genuinely healthy.
	v := metricsView{TotalAttempts: 200, Completed: 200, MeanQueueWaitMs: 1, P95LatencyMs: 600}
	if got := localStatus(v); got != StatusOK {
		t.Fatalf("localStatus = %s, want OK: slow but keeping up is not degraded", got)
	}
}

func TestLevelFor(t *testing.T) {
	t.Parallel()
	if levelFor(StatusFailing) != LevelCrit {
		t.Fatal("FAILING should be critical")
	}
	if levelFor(StatusDegraded) != LevelWarn {
		t.Fatal("DEGRADED should be warn")
	}
	if levelFor(StatusOK) != LevelInfo {
		t.Fatal("OK should be info")
	}
}
