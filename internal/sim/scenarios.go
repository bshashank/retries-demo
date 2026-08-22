package sim

import "fmt"

// Scenario preset names.
const (
	ScenarioNominal        = "nominal"
	ScenarioSASTSlow       = "sast-slow"
	ScenarioArtifactOutage = "artifact-outage"
	ScenarioKafkaLag       = "kafka-lag"
)

// scenarioDefs is the catalogue. Expected text is written so a reviewer can
// check the claim against the running graph rather than take it on trust.
var scenarioDefs = []ScenarioInfo{
	{
		Name:        ScenarioNominal,
		Label:       "Nominal",
		Description: "Clear every injection and restore default edge classifications. Identical to Reset.",
		Expected:    "All nine nodes settle to OK within a few seconds as queues drain and the 5s metrics window ages out. Queue depths return to ~0, run success rate to ~100%.",
	},
	{
		Name:        ScenarioSASTSlow,
		Label:       "SAST Engine Slowdown (10x)",
		Description: "The SAST engine takes 10x its normal 60ms. Security Scan depends on it essentially, but the orchestrator treats Security Scan as non-essential with a 300ms budget.",
		Expected:    "SAST Engine and Security Scan both go red. Global health settles at DEGRADED and stays there - it must never reach FAILING. The containment proof is on the orchestrator: its queue depth and mean queue wait stay at baseline, because the 300ms timeout caps what a non-essential branch can cost it.",
	},
	{
		Name:        ScenarioArtifactOutage,
		Label:       "Artifact Store Outage (10x)",
		Description: "The artifact store takes 10x its normal 25ms. It is the shared leaf: build and test both depend on it essentially, so it absorbs their combined ~40 req/sec through one queue.",
		Expected:    "Artifact Store saturates (offered load ~40/sec against ~32/sec of capacity), its queue climbs and starts shedding. Build and test block on it, so both back up too, and global health reaches FAILING within roughly 10-20 seconds. This is the escalation path a non-essential failure cannot take.",
	},
	{
		Name:        ScenarioKafkaLag,
		Label:       "Kafka Bus Lag (5x)",
		Description: "The Kafka event bus takes 5x its normal 25ms, pushing its 2 consumers past saturation. Telemetry depends on it essentially; the orchestrator treats telemetry as non-essential.",
		Expected:    "Kafka's queue grows monotonically until the 300ms non-essential budget starts abandoning queued work, then holds at that ceiling with mean queue wait pinned near 300ms. Telemetry degrades and goes red. Global health is DEGRADED, and again the orchestrator's own queue does not move.",
	},
}

// Scenarios lists the available presets.
func (e *Engine) Scenarios() []ScenarioInfo {
	out := make([]ScenarioInfo, len(scenarioDefs))
	copy(out, scenarioDefs)
	return out
}

// ApplyScenario applies a named preset.
//
// Every scenario starts from a clean slate (injections cleared, edges restored)
// so a preset always reproduces the behaviour its Expected text describes,
// regardless of what the operator was fiddling with beforehand.
func (e *Engine) ApplyScenario(name string) error {
	var (
		target string
		mult   float64
		label  string
	)
	switch name {
	case ScenarioNominal:
	case ScenarioSASTSlow:
		target, mult = NodeSAST, 10
	case ScenarioArtifactOutage:
		target, mult = NodeArtifactStore, 10
	case ScenarioKafkaLag:
		target, mult = NodeKafka, 5
	default:
		return fmt.Errorf("sim: unknown scenario %q", name)
	}
	for _, s := range scenarioDefs {
		if s.Name == name {
			label = s.Label
			break
		}
	}

	e.reset()
	level := LevelInfo
	if target != "" {
		e.services[target].latencyMultiplier.Store(mult)
		level = LevelWarn
	}
	e.emit(level, fmt.Sprintf("scenario applied: %s", label))
	e.kick()
	return nil
}
