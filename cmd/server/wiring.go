package main

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"pipelinehealth/internal/sim"
)

// newSimController builds the simulation engine and returns it behind the
// sim.Controller interface plus a shutdown func.
//
// ============================== ADJUST ME ===================================
// The simulation package is being written in parallel. Once it exposes its
// engine, replace the placeholder block below with the single real line:
//
//	e := sim.New()               // or sim.NewEngine()
//	return e, func() { e.Close() } // or e.Stop()
//
// Nothing else in this command depends on the concrete type: main.go and the
// whole api package speak only sim.Controller.
// ============================================================================
func newSimController(logger *slog.Logger) (sim.Controller, func()) {
	logger.Warn("using the placeholder simulation stub; wire cmd/server/wiring.go to the real engine")
	p := newPlaceholderController()
	return p, p.Close
}

// placeholderController is a temporary stand-in that satisfies sim.Controller so
// the binary, the container image, and the static/SSE plumbing can be exercised
// end to end before the engine lands. Delete this whole file's placeholder
// section when wiring the real engine.
type placeholderController struct {
	mu      sync.Mutex
	started time.Time
	nodes   []sim.NodeSnapshot
	edges   []sim.EdgeSnapshot
	events  []sim.Event
}

func newPlaceholderController() *placeholderController {
	ids := []string{
		sim.NodeOrchestrator, sim.NodeBuild, sim.NodeTest, sim.NodeSecurityScan,
		sim.NodeTelemetry, sim.NodeArtifactStore, sim.NodeRegistry, sim.NodeSAST, sim.NodeKafka,
	}
	nodes := make([]sim.NodeSnapshot, 0, len(ids))
	for i, id := range ids {
		nodes = append(nodes, sim.NodeSnapshot{
			ID: id, Label: id, Tier: i / 3,
			LocalStatus: sim.StatusOK, RollupStatus: sim.StatusOK,
			QueueCapacity: 100, Workers: 4, LatencyMultiplier: 1,
		})
	}
	return &placeholderController{
		started: time.Now(),
		nodes:   nodes,
		edges:   []sim.EdgeSnapshot{},
		events: []sim.Event{{
			ID: 1, AtMs: time.Now().UnixMilli(), Level: sim.LevelInfo,
			Message: "placeholder controller active; real simulation not yet wired",
		}},
	}
}

func (p *placeholderController) Snapshot() sim.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return sim.Snapshot{
		AtMs:           time.Now().UnixMilli(),
		Global:         sim.StatusOK,
		RunsPerSec:     0,
		RunSuccessRate: 1,
		Nodes:          append([]sim.NodeSnapshot(nil), p.nodes...),
		Edges:          append([]sim.EdgeSnapshot(nil), p.edges...),
		Events:         append([]sim.Event(nil), p.events...),
	}
}

func (p *placeholderController) Inject(nodeID string, latencyMultiplier, failRate float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.nodes {
		if p.nodes[i].ID == nodeID {
			p.nodes[i].LatencyMultiplier = latencyMultiplier
			p.nodes[i].FailRate = failRate
			return nil
		}
	}
	return errors.New("unknown node: " + nodeID)
}

func (p *placeholderController) SetEdgeEssential(from, to string, essential bool) error {
	return errors.New("unknown edge: " + from + " -> " + to)
}

func (p *placeholderController) ApplyScenario(name string) error {
	return errors.New("unknown scenario: " + name)
}

func (p *placeholderController) Reset() {}

func (p *placeholderController) Scenarios() []sim.ScenarioInfo { return []sim.ScenarioInfo{} }

func (p *placeholderController) Close() {}
