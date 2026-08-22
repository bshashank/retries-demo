package sim

import (
	"testing"
	"time"
)

func TestTopologyShape(t *testing.T) {
	t.Parallel()

	nodes, edges := nodeSpecs(), edgeSpecs()
	if len(nodes) != 9 {
		t.Fatalf("got %d nodes, want 9", len(nodes))
	}
	if len(edges) != 9 {
		t.Fatalf("got %d edges, want 9", len(edges))
	}

	wantTiers := map[string]int{
		NodeOrchestrator:  1,
		NodeBuild:         2,
		NodeTest:          2,
		NodeSecurityScan:  2,
		NodeTelemetry:     2,
		NodeArtifactStore: 3,
		NodeRegistry:      3,
		NodeSAST:          3,
		NodeKafka:         3,
	}
	wantLabels := map[string]string{
		NodeOrchestrator:  "Pipeline Orchestrator",
		NodeBuild:         "Build & Compile",
		NodeTest:          "Test Suite",
		NodeSecurityScan:  "Security Scan",
		NodeTelemetry:     "Telemetry Reporter",
		NodeArtifactStore: "Artifact Store",
		NodeRegistry:      "Container Registry",
		NodeSAST:          "SAST Engine",
		NodeKafka:         "Kafka Event Bus",
	}

	byID := map[string]nodeSpec{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if len(byID) != len(nodes) {
		t.Fatal("duplicate node IDs")
	}
	for id, tier := range wantTiers {
		n, ok := byID[id]
		if !ok {
			t.Fatalf("node %q missing", id)
		}
		if n.Tier != tier {
			t.Errorf("node %q tier = %d, want %d", id, n.Tier, tier)
		}
		if n.Label != wantLabels[id] {
			t.Errorf("node %q label = %q, want %q", id, n.Label, wantLabels[id])
		}
		if n.BaseLatency <= 0 || n.Workers <= 0 || n.QueueCap <= 0 {
			t.Errorf("node %q has non-positive tuning: %+v", id, n)
		}
	}
}

func TestTopologyEdges(t *testing.T) {
	t.Parallel()

	want := map[[2]string]bool{ // edge -> essential by default
		{NodeOrchestrator, NodeBuild}:        true,
		{NodeOrchestrator, NodeTest}:         true,
		{NodeOrchestrator, NodeSecurityScan}: false,
		{NodeOrchestrator, NodeTelemetry}:    false,
		{NodeBuild, NodeArtifactStore}:       true,
		{NodeBuild, NodeRegistry}:            false,
		{NodeTest, NodeArtifactStore}:        true,
		{NodeSecurityScan, NodeSAST}:         true,
		{NodeTelemetry, NodeKafka}:           true,
	}

	got := map[[2]string]bool{}
	for _, e := range edgeSpecs() {
		got[[2]string{e.From, e.To}] = e.Essential
	}
	if len(got) != len(want) {
		t.Fatalf("got %d distinct edges, want %d", len(got), len(want))
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("edge %s->%s missing", k[0], k[1])
			continue
		}
		if gv != v {
			t.Errorf("edge %s->%s essential = %v, want %v", k[0], k[1], gv, v)
		}
	}
}

// artifact-store is the shared leaf that makes artifact-outage escalate: it
// takes build's and test's combined load through one queue.
func TestArtifactStoreHasTwoEssentialParents(t *testing.T) {
	t.Parallel()

	var parents []string
	for _, e := range edgeSpecs() {
		if e.To != NodeArtifactStore {
			continue
		}
		if !e.Essential {
			t.Errorf("edge %s->artifact-store should default to essential", e.From)
		}
		parents = append(parents, e.From)
	}
	if len(parents) != 2 {
		t.Fatalf("artifact-store has %d parents (%v), want exactly 2", len(parents), parents)
	}
	seen := map[string]bool{parents[0]: true, parents[1]: true}
	if !seen[NodeBuild] || !seen[NodeTest] {
		t.Fatalf("artifact-store parents = %v, want build and test", parents)
	}
}

func TestValidateTopologyAcceptsDefault(t *testing.T) {
	t.Parallel()
	if err := validateTopology(nodeSpecs(), edgeSpecs()); err != nil {
		t.Fatalf("default topology invalid: %v", err)
	}
}

func TestValidateTopologyRejectsBadGraphs(t *testing.T) {
	t.Parallel()

	base := []nodeSpec{
		{"a", "A", 1, time.Millisecond, 1, 1},
		{"b", "B", 2, time.Millisecond, 1, 1},
		{"c", "C", 3, time.Millisecond, 1, 1},
	}

	tests := []struct {
		name  string
		nodes []nodeSpec
		edges []edgeSpec
	}{
		{
			name:  "unknown edge target",
			nodes: base,
			edges: []edgeSpec{{"a", "nope", true}},
		},
		{
			name:  "unknown edge source",
			nodes: base,
			edges: []edgeSpec{{"nope", "b", true}},
		},
		{
			name:  "self edge",
			nodes: base,
			edges: []edgeSpec{{"a", "a", true}},
		},
		{
			name:  "duplicate edge",
			nodes: base,
			edges: []edgeSpec{{"a", "b", true}, {"a", "b", false}},
		},
		{
			name:  "duplicate node",
			nodes: append(append([]nodeSpec{}, base...), base[0]),
			edges: nil,
		},
		{
			name:  "edge climbs a tier",
			nodes: base,
			edges: []edgeSpec{{"c", "a", true}},
		},
		{
			name: "cycle",
			nodes: []nodeSpec{
				{"a", "A", 1, time.Millisecond, 1, 1},
				{"b", "B", 1, time.Millisecond, 1, 1},
			},
			edges: []edgeSpec{{"a", "b", true}, {"b", "a", true}},
		},
		{
			name:  "zero workers",
			nodes: []nodeSpec{{"a", "A", 1, time.Millisecond, 0, 1}},
			edges: nil,
		},
		{
			name:  "zero queue capacity",
			nodes: []nodeSpec{{"a", "A", 1, time.Millisecond, 1, 0}},
			edges: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTopology(tc.nodes, tc.edges); err == nil {
				t.Fatalf("validateTopology accepted an invalid graph (%s)", tc.name)
			}
		})
	}
}

// The graph must be acyclic, and topoOrder is both the proof and the shutdown
// order the engine relies on.
func TestTopoOrderIsParentsFirst(t *testing.T) {
	t.Parallel()

	nodes, edges := nodeSpecs(), edgeSpecs()
	order, err := topoOrder(nodes, edges)
	if err != nil {
		t.Fatalf("topoOrder: %v", err)
	}
	if len(order) != len(nodes) {
		t.Fatalf("topoOrder returned %d ids, want %d", len(order), len(nodes))
	}

	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, e := range edges {
		if pos[e.From] >= pos[e.To] {
			t.Errorf("edge %s->%s: parent at %d must precede child at %d", e.From, e.To, pos[e.From], pos[e.To])
		}
	}
	if order[0] != NodeOrchestrator {
		t.Errorf("order[0] = %q, want the root orchestrator", order[0])
	}

	// Leaves-first is exactly the reverse.
	eval := reversed(order)
	evalPos := map[string]int{}
	for i, id := range eval {
		evalPos[id] = i
	}
	for _, e := range edges {
		if evalPos[e.To] >= evalPos[e.From] {
			t.Errorf("eval order: child %s must be evaluated before parent %s", e.To, e.From)
		}
	}
}

func TestTopoOrderDetectsCycle(t *testing.T) {
	t.Parallel()
	nodes := []nodeSpec{
		{"a", "A", 1, time.Millisecond, 1, 1},
		{"b", "B", 1, time.Millisecond, 1, 1},
		{"c", "C", 1, time.Millisecond, 1, 1},
	}
	edges := []edgeSpec{{"a", "b", true}, {"b", "c", true}, {"c", "a", true}}
	if _, err := topoOrder(nodes, edges); err == nil {
		t.Fatal("topoOrder accepted a cycle")
	}
}

// Every edge endpoint must resolve to a real service once the engine is built.
func TestEngineWiresEveryEdge(t *testing.T) {
	t.Parallel()

	e := New()
	defer e.Close()

	if len(e.services) != 9 {
		t.Fatalf("engine has %d services, want 9", len(e.services))
	}
	if len(e.edges) != 9 {
		t.Fatalf("engine has %d edges, want 9", len(e.edges))
	}
	for _, d := range e.edges {
		if d.child == nil {
			t.Fatalf("edge %s->%s has a nil child", d.from, d.to)
		}
		if d.child.id != d.to {
			t.Fatalf("edge %s->%s wired to %q", d.from, d.to, d.child.id)
		}
		if _, ok := e.services[d.from]; !ok {
			t.Fatalf("edge source %q is not a service", d.from)
		}
		if d.timeout != nonEssentialTimeout {
			t.Fatalf("edge %s->%s timeout = %v, want %v", d.from, d.to, d.timeout, nonEssentialTimeout)
		}
	}

	// Every node reachable from the root.
	seen := map[string]bool{}
	var walk func(s *service)
	walk = func(s *service) {
		if seen[s.id] {
			return
		}
		seen[s.id] = true
		for _, d := range s.deps {
			walk(d.child)
		}
	}
	walk(e.root)
	if len(seen) != 9 {
		t.Fatalf("only %d of 9 nodes reachable from the orchestrator: %v", len(seen), seen)
	}
}

func TestSnapshotExposesTopology(t *testing.T) {
	t.Parallel()

	e := New()
	defer e.Close()

	snap := e.Snapshot()
	if len(snap.Nodes) != 9 {
		t.Fatalf("snapshot has %d nodes, want 9", len(snap.Nodes))
	}
	if len(snap.Edges) != 9 {
		t.Fatalf("snapshot has %d edges, want 9", len(snap.Edges))
	}

	ids := map[string]NodeSnapshot{}
	for _, n := range snap.Nodes {
		ids[n.ID] = n
	}
	for _, e := range snap.Edges {
		if _, ok := ids[e.From]; !ok {
			t.Errorf("edge source %q not in nodes", e.From)
		}
		if _, ok := ids[e.To]; !ok {
			t.Errorf("edge target %q not in nodes", e.To)
		}
		if e.TimeoutMs != 300 {
			t.Errorf("edge %s->%s timeoutMs = %v, want 300", e.From, e.To, e.TimeoutMs)
		}
	}
	orch := ids[NodeOrchestrator]
	if orch.Workers != 32 || orch.QueueCapacity != 128 {
		t.Errorf("orchestrator workers/cap = %d/%d, want 32/128", orch.Workers, orch.QueueCapacity)
	}
	if orch.LatencyMultiplier != 1 || orch.FailRate != 0 {
		t.Errorf("orchestrator starts injected: mult=%v fail=%v", orch.LatencyMultiplier, orch.FailRate)
	}
}
