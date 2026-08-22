/**
 * Wire types. These mirror `internal/sim/contract.go` exactly — that file is the
 * frozen contract between the simulation core, the HTTP layer and this app.
 * Field names match the Go struct JSON tags one-for-one.
 */

/** Go: sim.Status */
export type Status = 'OK' | 'DEGRADED' | 'FAILING'

/** Go: sim.EventLevel */
export type EventLevel = 'info' | 'warn' | 'critical'

/** Go: sim.NodeSnapshot */
export interface NodeSnapshot {
  id: string
  label: string
  tier: number

  /** Reflects only this node's own saturation and errors. */
  localStatus: Status
  /** Folds in dependency health via the essential/non-essential rules. Headline state. */
  rollupStatus: Status

  queueDepth: number
  queueCapacity: number
  inFlight: number
  workers: number

  throughputPerSec: number
  errorRate: number
  rejectRate: number
  abandonRate: number

  meanQueueWaitMs: number
  p50LatencyMs: number
  p95LatencyMs: number

  baseLatencyMs: number
  latencyMultiplier: number
  failRate: number
}

/** Go: sim.EdgeSnapshot. `essential` is runtime-toggleable. */
export interface EdgeSnapshot {
  from: string
  to: string
  essential: boolean
  timeoutMs: number
}

/**
 * Go: sim.Event. Named `SimEvent` here purely to avoid shadowing the DOM `Event`
 * global; the wire shape is unchanged.
 */
export interface SimEvent {
  id: number
  atMs: number
  level: EventLevel
  message: string
}

/** Go: sim.Snapshot — the complete state pushed over SSE. */
export interface Snapshot {
  atMs: number
  global: Status

  /** Pipeline-run level metrics, measured at the orchestrator entry point. */
  runsPerSec: number
  runSuccessRate: number
  runP95Ms: number

  nodes: NodeSnapshot[]
  edges: EdgeSnapshot[]
  events: SimEvent[]
}

/** Go: sim.ScenarioInfo — returned by GET /api/scenarios. */
export interface ScenarioInfo {
  name: string
  label: string
  description: string
  expected: string
}

/** Request body for POST /api/inject. */
export interface InjectRequest {
  nodeId: string
  latencyMultiplier: number
  failRate: number
}

/** Request body for POST /api/edge. */
export interface EdgeRequest {
  from: string
  to: string
  essential: boolean
}

/** Request body for POST /api/scenario. */
export interface ScenarioRequest {
  name: string
}
