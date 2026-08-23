/**
 * DEV / TEST ONLY — a self-contained stand-in for the Go simulation.
 *
 * This exists so the dashboard can be developed and demoed before the backend is
 * running, and so tests have a deterministic snapshot source. It is reached only
 * via the `?mock=1` query parameter (see `isMockMode`) and from tests; nothing in
 * the production render path imports anything except through that switch.
 *
 * The queueing maths here is a caricature — the real engine runs actual worker
 * pools. What it does reproduce faithfully is the *shape* of the wire contract,
 * the mode-based rollup rule, and (heuristically) the RC/Normal shedding split
 * that the demo is about. Scenario names and target nodes mirror the real
 * backend's five presets exactly, so mock mode tells the same story as live mode.
 */
import type {
  DependencyMode,
  EdgeSnapshot,
  NodeSnapshot,
  ScenarioInfo,
  SimEvent,
  Snapshot,
  Status,
} from '../types'
import type { SimApi } from '../lib/api'
import type { StreamTransport } from '../lib/transport'
import { statusSeverity, worstStatus } from '../lib/status'

export function isMockMode(search: string = globalThis.location?.search ?? ''): boolean {
  const value = new URLSearchParams(search).get('mock')
  return value !== null && value !== '0' && value !== 'false'
}

interface NodeSpec {
  id: string
  label: string
  tier: number
  workers: number
  queueCapacity: number
  baseLatencyMs: number
  /** Nominal arrival rate in requests/sec at full pipeline throughput. */
  arrivalPerSec: number
  /** Set only on the two gated resources (SAST Engine, Container Registry). */
  isGate?: boolean
}

const NODE_SPECS: readonly NodeSpec[] = [
  { id: 'orchestrator', label: 'Pipeline Orchestrator', tier: 1, workers: 4, queueCapacity: 256, baseLatencyMs: 25, arrivalPerSec: 20 },
  { id: 'build', label: 'Build & Compile', tier: 2, workers: 6, queueCapacity: 192, baseLatencyMs: 120, arrivalPerSec: 20 },
  { id: 'test', label: 'Test Suite', tier: 2, workers: 6, queueCapacity: 192, baseLatencyMs: 150, arrivalPerSec: 20 },
  { id: 'security-scan', label: 'Security Scan', tier: 2, workers: 4, queueCapacity: 128, baseLatencyMs: 90, arrivalPerSec: 20 },
  { id: 'telemetry', label: 'Telemetry Reporter', tier: 2, workers: 3, queueCapacity: 128, baseLatencyMs: 40, arrivalPerSec: 20 },
  { id: 'artifact-store', label: 'Artifact Store', tier: 3, workers: 8, queueCapacity: 256, baseLatencyMs: 35, arrivalPerSec: 40 },
  { id: 'container-registry', label: 'Container Registry', tier: 3, workers: 4, queueCapacity: 128, baseLatencyMs: 60, arrivalPerSec: 20, isGate: true },
  { id: 'sast-engine', label: 'SAST Engine', tier: 3, workers: 6, queueCapacity: 96, baseLatencyMs: 180, arrivalPerSec: 20, isGate: true },
  { id: 'kafka-bus', label: 'Kafka Event Bus', tier: 3, workers: 3, queueCapacity: 192, baseLatencyMs: 40, arrivalPerSec: 20 },
]

interface EdgeSpec {
  from: string
  to: string
  mode: DependencyMode
  timeoutMs: number
}

const EDGE_SPECS: readonly EdgeSpec[] = [
  { from: 'orchestrator', to: 'build', mode: 'blocking', timeoutMs: 2000 },
  { from: 'orchestrator', to: 'test', mode: 'blocking', timeoutMs: 3000 },
  { from: 'orchestrator', to: 'security-scan', mode: 'blocking', timeoutMs: 2000 },
  { from: 'orchestrator', to: 'telemetry', mode: 'best_effort', timeoutMs: 300 },
  { from: 'build', to: 'artifact-store', mode: 'blocking', timeoutMs: 1200 },
  { from: 'build', to: 'container-registry', mode: 'gated', timeoutMs: 300 },
  { from: 'test', to: 'artifact-store', mode: 'blocking', timeoutMs: 1200 },
  { from: 'security-scan', to: 'sast-engine', mode: 'gated', timeoutMs: 300 },
  { from: 'telemetry', to: 'kafka-bus', mode: 'blocking', timeoutMs: 500 },
]

const MOCK_SCENARIOS: readonly ScenarioInfo[] = [
  {
    name: 'nominal',
    label: 'Nominal',
    description: 'Clear every injection and restore default edge classifications. Identical to Reset.',
    expected: 'All nine nodes settle to OK within a few seconds.',
  },
  {
    name: 'sast-slow',
    label: 'SAST Engine Slowdown (10x)',
    description: 'The SAST engine takes 10x its normal latency. Security Scan holds calls in a bounded backlog instead of blocking or skipping them.',
    expected: 'SAST Engine and Security Scan degrade first while the hold queue has headroom. Left running, the backlog saturates and global health escalates to FAILING — but release-candidate runs keep succeeding while Normal-priority runs get shed.',
  },
  {
    name: 'registry-slow',
    label: 'Container Registry Slowdown (10x)',
    description: 'The container registry takes 10x its normal latency. Build holds calls through the same gated pattern as SAST.',
    expected: 'The same two-stage story as the SAST scenario, on a different branch of the graph.',
  },
  {
    name: 'artifact-outage',
    label: 'Artifact Store Outage (10x)',
    description: 'The artifact store takes 10x its normal latency. Build and Test both depend on it as a blocking call.',
    expected: 'Artifact Store saturates, Build and Test back up behind it, and global health reaches FAILING quickly — there is no hold queue on this path to absorb the slowdown first.',
  },
  {
    name: 'kafka-lag',
    label: 'Kafka Bus Lag (5x)',
    description: 'The Kafka event bus takes 5x its normal latency. Telemetry depends on it, but the orchestrator treats Telemetry as best-effort.',
    expected: 'Telemetry degrades and goes red, but the orchestrator times out after 300ms and keeps serving. Global health stays DEGRADED.',
  },
]

interface Injection {
  latencyMultiplier: number
  failRate: number
}

const edgeKey = (from: string, to: string): string => `${from}->${to}`

/** Best-effort dependencies cost you one severity level, not the whole run. */
function demote(status: Status): Status {
  if (status === 'FAILING') return 'DEGRADED'
  if (status === 'DEGRADED') return 'OK'
  return 'OK'
}

/** Cheap deterministic PRNG so tests never flake on jitter. */
function makeRandom(seed: number): () => number {
  let s = seed >>> 0
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0
    return s / 0x100000000
  }
}

export interface MockEngine {
  api: SimApi
  transport: StreamTransport
  /** Produce a snapshot immediately, without running the timer. */
  snapshot: () => Snapshot
  /** Advance the simulated clock by one 200ms tick. */
  tick: () => Snapshot
  scenarios: readonly ScenarioInfo[]
}

export interface MockEngineOptions {
  seed?: number
  intervalMs?: number
}

export function createMockEngine(options: MockEngineOptions = {}): MockEngine {
  const { seed = 20260822, intervalMs = 200 } = options
  const random = makeRandom(seed)

  const injections = new Map<string, Injection>()
  const modes = new Map<string, DependencyMode>(EDGE_SPECS.map((e) => [edgeKey(e.from, e.to), e.mode]))
  const gateNodes = new Set(NODE_SPECS.filter((n) => n.isGate).map((n) => n.id))
  const lastRollup = new Map<string, Status>()
  let lastGlobal: Status = 'OK'
  let events: SimEvent[] = []
  let eventId = 1
  let ticks = 0
  const startedAt = Date.now()

  const pushEvent = (level: SimEvent['level'], message: string): void => {
    events = [...events.slice(-99), { id: eventId++, atMs: Date.now(), level, message }]
  }

  const injectionFor = (id: string): Injection =>
    injections.get(id) ?? { latencyMultiplier: 1, failRate: 0 }

  const buildEdges = (): EdgeSnapshot[] =>
    EDGE_SPECS.map((e) => ({
      from: e.from,
      to: e.to,
      mode: modes.get(edgeKey(e.from, e.to)) ?? e.mode,
      supportsGated: gateNodes.has(e.to),
      timeoutMs: e.timeoutMs,
    }))

  const computeNode = (spec: NodeSpec, edges: EdgeSnapshot[]): NodeSnapshot => {
    const injection = injectionFor(spec.id)
    const jitter = 0.9 + random() * 0.2
    const effectiveLatency = spec.baseLatencyMs * injection.latencyMultiplier * jitter

    // Little's law: how much work can this pool retire per second?
    const serviceCapacity = spec.workers / (effectiveLatency / 1000)
    const utilization = spec.arrivalPerSec / Math.max(serviceCapacity, 0.001)

    const depthFraction =
      utilization < 0.85
        ? utilization * 0.08
        : Math.min(1, 0.068 + (utilization - 0.85) * 1.4)
    const queueDepth = Math.round(spec.queueCapacity * depthFraction)
    const inFlight = Math.min(spec.workers, Math.round(spec.workers * Math.min(1, utilization)))

    const meanQueueWaitMs = (queueDepth / Math.max(serviceCapacity, 0.001)) * 1000
    const p50 = effectiveLatency + meanQueueWaitMs
    const p95 = effectiveLatency * 2.1 + meanQueueWaitMs * 1.6

    const timeout = Math.min(
      ...edges.filter((e) => e.to === spec.id).map((e) => e.timeoutMs),
      Number.POSITIVE_INFINITY,
    )
    const abandonRate = Number.isFinite(timeout) && p95 > timeout
      ? Math.min(0.95, (p95 - timeout) / (timeout * 2))
      : 0
    const rejectRate = depthFraction >= 0.99 ? Math.min(0.9, (utilization - 1) * 0.4) : 0
    const errorRate = Math.min(1, injection.failRate + abandonRate * 0.5 + rejectRate * 0.5)
    const throughput = Math.max(0, Math.min(spec.arrivalPerSec, serviceCapacity) * (1 - errorRate))

    let localStatus: Status = 'OK'
    if (utilization >= 1.02 || depthFraction >= 0.95 || errorRate >= 0.25) localStatus = 'FAILING'
    else if (utilization >= 0.8 || depthFraction >= 0.5 || errorRate >= 0.05) localStatus = 'DEGRADED'

    return {
      id: spec.id,
      label: spec.label,
      tier: spec.tier,
      localStatus,
      rollupStatus: localStatus, // replaced by the rollup pass below
      queueDepth: Math.max(0, Math.min(spec.queueCapacity, queueDepth)),
      queueCapacity: spec.queueCapacity,
      inFlight,
      workers: spec.workers,
      throughputPerSec: throughput,
      errorRate,
      rejectRate: Math.max(0, rejectRate),
      abandonRate: Math.max(0, abandonRate),
      meanQueueWaitMs,
      p50LatencyMs: p50,
      p95LatencyMs: p95,
      baseLatencyMs: spec.baseLatencyMs,
      latencyMultiplier: injection.latencyMultiplier,
      failRate: injection.failRate,
    }
  }

  /**
   * Deepest tier first, so a parent always sees finished children. Blocking
   * and gated edges both propagate the child's status as-is (both are
   * "essential" — see modeEssential on the Go side); only best-effort demotes.
   * A gated dependency's graduated DEGRADED-then-FAILING pacing comes from its
   * own node-level utilization curve above, not from a special rollup rule —
   * mirroring how the real engine keeps rollup mode-agnostic too.
   */
  const applyRollup = (nodes: NodeSnapshot[], edges: EdgeSnapshot[]): void => {
    const byId = new Map(nodes.map((n) => [n.id, n]))
    const ordered = [...nodes].sort((a, b) => b.tier - a.tier)
    for (const node of ordered) {
      const contributions: Status[] = [node.localStatus]
      for (const edge of edges) {
        if (edge.from !== node.id) continue
        const child = byId.get(edge.to)
        if (!child) continue
        contributions.push(edge.mode === 'best_effort' ? demote(child.rollupStatus) : child.rollupStatus)
      }
      node.rollupStatus = worstStatus(contributions)
    }
  }

  const snapshot = (): Snapshot => {
    const edges = buildEdges()
    const nodes = NODE_SPECS.map((spec) => computeNode(spec, edges))
    applyRollup(nodes, edges)

    for (const node of nodes) {
      const previous = lastRollup.get(node.id)
      if (previous !== undefined && previous !== node.rollupStatus) {
        const worse = statusSeverity(node.rollupStatus) > statusSeverity(previous)
        pushEvent(
          node.rollupStatus === 'FAILING' ? 'critical' : worse ? 'warn' : 'info',
          `${node.label}: ${previous} -> ${node.rollupStatus}`,
        )
      }
      lastRollup.set(node.id, node.rollupStatus)
    }

    const orchestrator = nodes.find((n) => n.id === 'orchestrator')
    const global: Status = orchestrator?.rollupStatus ?? 'OK'
    if (global !== lastGlobal) {
      pushEvent(
        global === 'FAILING' ? 'critical' : global === 'DEGRADED' ? 'warn' : 'info',
        `Global status is now ${global}`,
      )
      lastGlobal = global
    }

    const successByStatus: Record<Status, number> = { OK: 0.995, DEGRADED: 0.928, FAILING: 0.19 }
    const rateByStatus: Record<Status, number> = { OK: 20.4, DEGRADED: 13.8, FAILING: 3.6 }
    const essentialP95 = nodes
      .filter((n) => n.tier <= 2)
      .reduce((max, n) => Math.max(max, n.p95LatencyMs), 0)

    // RC/Normal split: only a saturated gate (a gate node actively rejecting)
    // sheds Normal-priority traffic. Everything else — including a classic
    // blocking cascade — hits both classes equally, matching the real engine
    // (priority only ever gates admission into service.gatedCall).
    const gatedRejecting = nodes.some((n) => gateNodes.has(n.id) && n.rejectRate > 0.02)
    const runSuccessRate = Math.min(1, successByStatus[global] * (0.98 + random() * 0.04))
    const runSuccessRateRC = gatedRejecting ? Math.min(1, 0.97 + random() * 0.03) : runSuccessRate
    const runSuccessRateNormal = gatedRejecting
      ? Math.max(0, runSuccessRate - 0.15 - random() * 0.2)
      : runSuccessRate

    return {
      atMs: startedAt + ticks * intervalMs,
      global,
      runsPerSec: rateByStatus[global] * (0.96 + random() * 0.08),
      runSuccessRate,
      runP95Ms: (orchestrator?.p95LatencyMs ?? 0) + essentialP95,
      runSuccessRateRC,
      runSuccessRateNormal,
      nodes,
      edges,
      events,
    }
  }

  const tick = (): Snapshot => {
    ticks += 1
    return snapshot()
  }

  const api: SimApi = {
    async inject(nodeId, latencyMultiplier, failRate) {
      if (!NODE_SPECS.some((n) => n.id === nodeId)) throw new Error(`unknown node ${nodeId}`)
      injections.set(nodeId, { latencyMultiplier, failRate })
      const label = NODE_SPECS.find((n) => n.id === nodeId)?.label ?? nodeId
      pushEvent(
        latencyMultiplier > 1 || failRate > 0 ? 'warn' : 'info',
        latencyMultiplier > 1 || failRate > 0
          ? `Injected ${latencyMultiplier}x latency / ${(failRate * 100).toFixed(0)}% failures into ${label}`
          : `Cleared injection on ${label}`,
      )
    },
    async setEdgeMode(from, to, mode) {
      const key = edgeKey(from, to)
      if (!modes.has(key)) throw new Error(`unknown edge ${key}`)
      if (mode === 'gated' && !gateNodes.has(to)) {
        throw new Error(`edge ${key} is gated but target ${to} has no gate config`)
      }
      modes.set(key, mode)
      pushEvent('warn', `Edge ${from} -> ${to} reclassified as ${mode.toUpperCase()}`)
    },
    async applyScenario(name) {
      const scenario = MOCK_SCENARIOS.find((s) => s.name === name)
      if (!scenario) throw new Error(`unknown scenario ${name}`)
      injections.clear()
      for (const e of EDGE_SPECS) modes.set(edgeKey(e.from, e.to), e.mode)

      if (name === 'sast-slow') {
        injections.set('sast-engine', { latencyMultiplier: 10, failRate: 0 })
      } else if (name === 'registry-slow') {
        injections.set('container-registry', { latencyMultiplier: 10, failRate: 0 })
      } else if (name === 'artifact-outage') {
        injections.set('artifact-store', { latencyMultiplier: 10, failRate: 0 })
      } else if (name === 'kafka-lag') {
        injections.set('kafka-bus', { latencyMultiplier: 5, failRate: 0 })
      }
      pushEvent(name === 'nominal' ? 'info' : 'warn', `Scenario applied: ${scenario.label}`)
    },
    async reset() {
      injections.clear()
      for (const e of EDGE_SPECS) modes.set(edgeKey(e.from, e.to), e.mode)
      pushEvent('info', 'Simulation reset: injections cleared, edge classifications restored')
    },
    async scenarios() {
      return [...MOCK_SCENARIOS]
    },
  }

  const transport: StreamTransport = {
    label: 'MOCK',
    subscribe({ onOpen, onMessage }) {
      let stopped = false
      const open = setTimeout(() => {
        if (stopped) return
        onOpen()
        onMessage(JSON.stringify(snapshot()))
      }, 0)
      const timer = setInterval(() => {
        if (stopped) return
        onMessage(JSON.stringify(tick()))
      }, intervalMs)
      return () => {
        stopped = true
        clearTimeout(open)
        clearInterval(timer)
      }
    },
  }

  return { api, transport, snapshot, tick, scenarios: MOCK_SCENARIOS }
}

/** One plausible snapshot, used by component tests as a fixture base. */
export function createMockSnapshot(overrides: Partial<Snapshot> = {}): Snapshot {
  return { ...createMockEngine().snapshot(), ...overrides }
}

export { MOCK_SCENARIOS, NODE_SPECS, EDGE_SPECS }
