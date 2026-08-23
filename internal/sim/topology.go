package sim

import (
	"fmt"
	"time"
)

// Simulation-wide tuning constants.
const (
	// nonEssentialTimeout bounds every non-essential dependency call. It is the
	// single most important number in the model: it is what makes a non-essential
	// failure cost the caller a *bounded* amount of occupancy instead of an
	// unbounded one, which is why a non-essential outage cannot back up its parent.
	nonEssentialTimeout = 300 * time.Millisecond

	// runDeadline is the global deadline for one pipeline run.
	runDeadline = 2 * time.Second

	// metricsWindow is the rolling window all rates/percentiles are computed over.
	metricsWindow = 5 * time.Second

	// tickInterval is how often the engine recomputes statuses and republishes
	// the cached snapshot.
	tickInterval = 200 * time.Millisecond

	// loadRatePerSec is the mean pipeline-run arrival rate at the orchestrator.
	// Inter-arrival times are exponentially distributed around it.
	loadRatePerSec = 20.0

	// maxEvents bounds the event ring.
	maxEvents = 100

	// minAttempts is the smallest number of in-window attempts we are willing to
	// draw a status conclusion from. Below it a node reports OK (insufficient data)
	// rather than letting one unlucky sample paint the graph red.
	minAttempts = 3
)

// nodeSpec is the static tuning for one simulated service: how long its own work
// takes, how many workers it has, and how deep its queue is. Capacity is
// workers/baseLatency requests per second; everything above that queues, and
// everything above the queue is shed.
type nodeSpec struct {
	ID          string
	Label       string
	Tier        int
	BaseLatency time.Duration
	Workers     int
	QueueCap    int

	// GateHoldBudget is only set on the target of a ModeGated edge (SAST
	// Engine, Container Registry). Zero means "not a gate" — validateTopology
	// rejects any edge that points ModeGated at such a node. See
	// service.gatedCall: this is the detached, long-lived deadline a held
	// call gets once the short synchronous grace window elapses, standing in
	// for "hours" compressed to a demo timescale. QueueCap on a gated node
	// does double duty as its hold capacity — see gatedCall's admission
	// check, which is why these two nodes carry a much larger QueueCap than
	// their own throughput would otherwise need.
	GateHoldBudget time.Duration
}

// edgeSpec is a static dependency declaration. Mode is the *default*
// classification; it is runtime-toggleable via Controller.SetEdgeMode.
type edgeSpec struct {
	From string
	To   string
	Mode DependencyMode
}

// nodeSpecs returns the fixed 9-node topology tuning table.
//
// Sizing rationale at the nominal 20 runs/sec:
//   - orchestrator occupancy is ~10ms + max(child latency) ~= 90ms, so ~2 of 32
//     workers are busy. The deep worker pool is deliberate: it is the headroom
//     that lets the orchestrator absorb a 300ms non-essential stall without its
//     queue moving.
//   - artifact-store is the shared leaf: it sees build's 20/s *plus* test's 20/s
//     through one queue, so it is the node that tips first when slowed.
//   - kafka-bus is deliberately thin (2 workers). At 20/s x 25ms it idles at
//     rho=0.25, but the 5x kafka-lag injection pushes it past rho=1 so queue
//     growth is actually observable rather than absorbed.
func nodeSpecs() []nodeSpec {
	return []nodeSpec{
		{ID: NodeOrchestrator, Label: "Pipeline Orchestrator", Tier: 1, BaseLatency: 10 * time.Millisecond, Workers: 32, QueueCap: 128},
		{ID: NodeBuild, Label: "Build & Compile", Tier: 2, BaseLatency: 30 * time.Millisecond, Workers: 16, QueueCap: 64},
		{ID: NodeTest, Label: "Test Suite", Tier: 2, BaseLatency: 40 * time.Millisecond, Workers: 16, QueueCap: 64},
		{ID: NodeSecurityScan, Label: "Security Scan", Tier: 2, BaseLatency: 20 * time.Millisecond, Workers: 12, QueueCap: 64},
		{ID: NodeTelemetry, Label: "Telemetry Reporter", Tier: 2, BaseLatency: 15 * time.Millisecond, Workers: 12, QueueCap: 64},
		{ID: NodeArtifactStore, Label: "Artifact Store", Tier: 3, BaseLatency: 25 * time.Millisecond, Workers: 6, QueueCap: 64},
		// Registry and SAST are the two gated resources: QueueCap is sized as
		// a hold capacity (see nodeSpec.GateHoldBudget doc), not just short-
		// burst absorption, which is why it's an order of magnitude bigger
		// than every other node's.
		{ID: NodeRegistry, Label: "Container Registry", Tier: 3, BaseLatency: 30 * time.Millisecond, Workers: 3, QueueCap: 300, GateHoldBudget: 60 * time.Second},
		{ID: NodeSAST, Label: "SAST Engine", Tier: 3, BaseLatency: 60 * time.Millisecond, Workers: 6, QueueCap: 300, GateHoldBudget: 60 * time.Second},
		{ID: NodeKafka, Label: "Kafka Event Bus", Tier: 3, BaseLatency: 30 * time.Millisecond, Workers: 2, QueueCap: 64},
	}
}

// edgeSpecs returns the fixed 9-edge dependency set with default classifications.
//
// Orchestrator->SecurityScan is ModeBlocking, not ModeBestEffort: a pipeline
// that can't get a SAST result can't ship, so this genuinely is essential.
// That's safe specifically because SecurityScan's own call to SAST is
// ModeGated (see service.gatedCall) — Security Scan's response time stays
// bounded regardless of SAST's health, so blocking on it here never risks
// the orchestrator's own queue backing up. The same reasoning reclassifies
// Build->Registry: CD can't deploy without the image landing in the
// registry, so it is essential too, and safe for the same reason.
func edgeSpecs() []edgeSpec {
	return []edgeSpec{
		{NodeOrchestrator, NodeBuild, ModeBlocking},
		{NodeOrchestrator, NodeTest, ModeBlocking},
		{NodeOrchestrator, NodeSecurityScan, ModeBlocking},
		{NodeOrchestrator, NodeTelemetry, ModeBestEffort},
		{NodeBuild, NodeArtifactStore, ModeBlocking},
		{NodeBuild, NodeRegistry, ModeGated},
		{NodeTest, NodeArtifactStore, ModeBlocking},
		{NodeSecurityScan, NodeSAST, ModeGated},
		{NodeTelemetry, NodeKafka, ModeBlocking},
	}
}

// validateTopology checks the structural invariants the rest of the engine and
// the UI rely on: unique nodes, resolvable endpoints, no duplicate or self
// edges, strictly increasing tiers along every edge, and acyclicity.
func validateTopology(nodes []nodeSpec, edges []edgeSpec) error {
	byID := make(map[string]nodeSpec, len(nodes))
	for _, n := range nodes {
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("sim: duplicate node id %q", n.ID)
		}
		if n.Workers <= 0 {
			return fmt.Errorf("sim: node %q must have at least one worker", n.ID)
		}
		if n.QueueCap <= 0 {
			return fmt.Errorf("sim: node %q must have a positive queue capacity", n.ID)
		}
		if n.Tier <= 0 {
			return fmt.Errorf("sim: node %q must have a positive tier", n.ID)
		}
		byID[n.ID] = n
	}

	seen := make(map[[2]string]bool, len(edges))
	for _, e := range edges {
		from, ok := byID[e.From]
		if !ok {
			return fmt.Errorf("sim: edge %s->%s has unknown source", e.From, e.To)
		}
		to, ok := byID[e.To]
		if !ok {
			return fmt.Errorf("sim: edge %s->%s has unknown target", e.From, e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("sim: self edge on %q", e.From)
		}
		key := [2]string{e.From, e.To}
		if seen[key] {
			return fmt.Errorf("sim: duplicate edge %s->%s", e.From, e.To)
		}
		seen[key] = true
		if to.Tier <= from.Tier {
			return fmt.Errorf("sim: edge %s(tier %d)->%s(tier %d) does not descend a tier",
				e.From, from.Tier, e.To, to.Tier)
		}
		if e.Mode == ModeGated && to.GateHoldBudget <= 0 {
			return fmt.Errorf("sim: edge %s->%s is ModeGated but target %q has no gate config", e.From, e.To, e.To)
		}
	}

	if _, err := topoOrder(nodes, edges); err != nil {
		return err
	}
	return nil
}

// topoOrder returns node IDs ordered parents-first: every node appears before
// all of its dependencies. Reversing it gives leaves-first evaluation order.
//
// It doubles as the acyclicity check (Kahn's algorithm cannot drain a cycle)
// and as the shutdown order: closing a parent's queue and draining its workers
// before touching its children guarantees nothing can ever send on a closed
// channel.
func topoOrder(nodes []nodeSpec, edges []edgeSpec) ([]string, error) {
	indeg := make(map[string]int, len(nodes))
	adj := make(map[string][]string, len(nodes))
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		indeg[n.ID] = 0
		known[n.ID] = true
	}
	for _, e := range edges {
		if !known[e.From] || !known[e.To] {
			return nil, fmt.Errorf("sim: edge %s->%s references an unknown node", e.From, e.To)
		}
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}

	// Seed in declaration order so the result is deterministic.
	queue := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if indeg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	out := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		for _, child := range adj[id] {
			indeg[child]--
			if indeg[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if len(out) != len(nodes) {
		return nil, fmt.Errorf("sim: dependency graph contains a cycle (%d of %d nodes ordered)", len(out), len(nodes))
	}
	return out, nil
}

// reversed returns a copy of ids in reverse order (leaves-first from a
// parents-first topological order).
func reversed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}
