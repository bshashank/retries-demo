// Package sim contains the pipeline simulation: a DAG of services, each backed by a
// real worker pool, driven by a continuous load generator.
//
// This file is the FROZEN contract between the simulation core, the HTTP API layer,
// and the frontend. Types here define the JSON wire format.
package sim

// Status is the health of a single node, or of the system as a whole.
type Status string

const (
	StatusOK       Status = "OK"
	StatusDegraded Status = "DEGRADED"
	StatusFailing  Status = "FAILING"
)

// DependencyMode classifies one edge. It replaces a plain essential/non-
// essential boolean because a boolean silently conflates two questions: does
// a failure here propagate (correctness), and does this call block the
// caller (dispatch). Most edges genuinely only need the first question
// answered, but a release gate (SAST, the container registry) needs an
// answer that's essential for correctness yet must not block — hence a
// third mode instead of a second, independently toggleable flag.
type DependencyMode string

const (
	// ModeBlocking: essential, synchronous, unbounded latency pass-through.
	// The classic case — no build, no pipeline.
	ModeBlocking DependencyMode = "blocking"
	// ModeBestEffort: non-essential, bounded by nonEssentialTimeout, proceeds
	// and degrades on failure or timeout.
	ModeBestEffort DependencyMode = "best_effort"
	// ModeGated: essential, but dispatched through service.gatedCall instead
	// of a direct blocking call — a resolves-fast-when-healthy, holds-when-
	// slow, sheds-non-RC-when-saturated pattern. See service.go.
	ModeGated DependencyMode = "gated"
)

// modeEssential derives whether a mode's failure propagates to the caller.
// This is the only property rollup.go ever needs to know about a mode — it
// stays completely unaware that gated dependencies exist.
func modeEssential(m DependencyMode) bool { return m != ModeBestEffort }

// Node IDs. The topology is fixed.
const (
	NodeOrchestrator = "orchestrator"
	NodeBuild        = "build"
	NodeTest         = "test"
	NodeSecurityScan = "security-scan"
	NodeTelemetry    = "telemetry"
	NodeArtifactStore = "artifact-store"
	NodeRegistry     = "container-registry"
	NodeSAST         = "sast-engine"
	NodeKafka        = "kafka-bus"
)

// NodeSnapshot is the observable state of one service at a point in time.
type NodeSnapshot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Tier  int    `json:"tier"`

	// LocalStatus reflects only this node's own saturation and errors.
	// RollupStatus additionally folds in dependency health via the essential/
	// non-essential rules; it is what the UI displays as the node's headline state.
	LocalStatus  Status `json:"localStatus"`
	RollupStatus Status `json:"rollupStatus"`

	QueueDepth    int `json:"queueDepth"`
	QueueCapacity int `json:"queueCapacity"`
	InFlight      int `json:"inFlight"`
	Workers       int `json:"workers"`

	ThroughputPerSec float64 `json:"throughputPerSec"`
	ErrorRate        float64 `json:"errorRate"`
	RejectRate       float64 `json:"rejectRate"`
	AbandonRate      float64 `json:"abandonRate"`

	MeanQueueWaitMs float64 `json:"meanQueueWaitMs"`
	P50LatencyMs    float64 `json:"p50LatencyMs"`
	P95LatencyMs    float64 `json:"p95LatencyMs"`

	BaseLatencyMs     float64 `json:"baseLatencyMs"`
	LatencyMultiplier float64 `json:"latencyMultiplier"`
	FailRate          float64 `json:"failRate"`
}

// EdgeSnapshot is one dependency relationship. Mode is runtime-toggleable.
type EdgeSnapshot struct {
	From string         `json:"from"`
	To   string         `json:"to"`
	Mode DependencyMode `json:"mode"`
	// SupportsGated: true only for edges whose target has gate config (a
	// gateHoldBudget), i.e. the edges ModeGated is physically meaningful for.
	// The UI uses this to decide whether to offer a "gated" option at all.
	SupportsGated bool    `json:"supportsGated"`
	TimeoutMs     float64 `json:"timeoutMs"`
}

// EventLevel maps to UI severity colouring.
type EventLevel string

const (
	LevelInfo EventLevel = "info"
	LevelWarn EventLevel = "warn"
	LevelCrit EventLevel = "critical"
)

// Event records a status transition or operator action.
type Event struct {
	ID      uint64     `json:"id"`
	AtMs    int64      `json:"atMs"`
	Level   EventLevel `json:"level"`
	Message string     `json:"message"`
}

// Snapshot is the complete state pushed to clients over SSE.
type Snapshot struct {
	AtMs   int64  `json:"atMs"`
	Global Status `json:"global"`

	// Pipeline-run level metrics, measured at the orchestrator entry point.
	RunsPerSec     float64 `json:"runsPerSec"`
	RunSuccessRate float64 `json:"runSuccessRate"`
	RunP95Ms       float64 `json:"runP95Ms"`

	// RunSuccessRateRC / RunSuccessRateNormal split the same success rate by
	// priority. This is the measured proof of the whole release-gate
	// mechanism: only Normal-priority runs can ever be shed, so a saturated
	// gate should show RunSuccessRateNormal dropping while RunSuccessRateRC
	// stays high — even while the global banner reads FAILING.
	RunSuccessRateRC     float64 `json:"runSuccessRateRC"`
	RunSuccessRateNormal float64 `json:"runSuccessRateNormal"`

	Nodes  []NodeSnapshot `json:"nodes"`
	Edges  []EdgeSnapshot `json:"edges"`
	Events []Event        `json:"events"`
}

// Controller is the surface the HTTP layer drives. Implemented by *Engine.
type Controller interface {
	// Snapshot returns the most recent computed state. Safe for concurrent use.
	Snapshot() Snapshot

	// Inject sets a latency multiplier (1.0 = normal) and an injected failure
	// probability (0..1) on a node. Returns an error for an unknown node ID.
	Inject(nodeID string, latencyMultiplier, failRate float64) error

	// SetEdgeMode reclassifies a dependency at runtime. Returns an error for
	// an unknown edge, or for ModeGated on an edge whose target has no gate
	// config.
	SetEdgeMode(from, to string, mode DependencyMode) error

	// ApplyScenario applies a named preset. Returns an error for an unknown name.
	ApplyScenario(name string) error

	// Reset clears all injections and restores default edge classifications.
	// Queues and metrics recover naturally rather than being force-cleared.
	Reset()

	// Scenarios lists the available preset names with human-readable descriptions.
	Scenarios() []ScenarioInfo
}

// ScenarioInfo describes a preset for the UI.
type ScenarioInfo struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Expected    string `json:"expected"`
}
