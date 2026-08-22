/**
 * DEV / TEST ONLY — a self-contained stand-in for the Go simulation.
 *
 * This exists so the dashboard can be developed and demoed before the backend is
 * running, and so tests have a deterministic snapshot source. It is reached only
 * via the `?mock=1` query parameter (see `isMockMode`) and from tests; nothing in
 * the production render path imports anything except through that switch.
 *
 * The queueing maths here is a caricature — the real engine runs actual worker
 * pools. What it does reproduce faithfully is the *shape* of the wire contract
 * and the essential / non-essential rollup rule that the demo is about.
 */
import type {
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
}

const NODE_SPECS: readonly NodeSpec[] = [
  { id: 'orchestrator', label: 'Pipeline Orchestrator', tier: 1, workers: 4, queueCapacity: 256, baseLatencyMs: 25, arrivalPerSec: 20 },
  { id: 'build', label: 'Build & Compile', tier: 2, workers: 6, queueCapacity: 192, baseLatencyMs: 120, arrivalPerSec: 20 },
  { id: 'test', label: 'Test Suite', tier: 2, workers: 6, queueCapacity: 192, baseLatencyMs: 150, arrivalPerSec: 20 },
  { id: 'security-scan', label: 'Security Scan', tier: 2, workers: 4, queueCapacity: 128, baseLatencyMs: 90, arrivalPerSec: 20 },
  { id: 'telemetry', label: 'Telemetry Reporter', tier: 2, workers: 3, queueCapacity: 128, baseLatencyMs: 40, arrivalPerSec: 20 },
  { id: 'artifact-store', label: 'Artifact Store', tier: 3, workers: 8, queueCapacity: 256, baseLatencyMs: 35, arrivalPerSec: 40 },
  { id: 'container-registry', label: 'Container Registry', tier: 3, workers: 4, queueCapacity: 128, baseLatencyMs: 60, arrivalPerSec: 20 },
  { id: 'sast-engine', label: 'SAST Engine', tier: 3, workers: 6, queueCapacity: 96, baseLatencyMs: 180, arrivalPerSec: 20 },
  { id: 'kafka-bus', label: 'Kafka Event Bus', tier: 3, workers: 3, queueCapacity: 192, baseLatencyMs: 40, arrivalPerSec: 20 },
]

interface EdgeSpec {
  from: string
  to: string
  essential: boolean
  timeoutMs: number
}

const EDGE_SPECS: readonly EdgeSpec[] = [
  { from: 'orchestrator', to: 'build', essential: true, timeoutMs: 2000 },
  { from: 'orchestrator', to: 'test', essential: true, timeoutMs: 3000 },
  { from: 'orchestrator', to: 'security-scan', essential: false, timeoutMs: 1500 },
  { from: 'orchestrator', to: 'telemetry', essential: false, timeoutMs: 800 },
  { from: 'build', to: 'artifact-store', essential: true, timeoutMs: 1200 },
  { from: 'build', to: 'container-registry', essential: false, timeoutMs: 1200 },
  { from: 'test', to: 'artifact-store', essential: true, timeoutMs: 1200 },
  { from: 'security-scan', to: 'sast-engine', essential: true, timeoutMs: 2500 },
  { from: 'telemetry', to: 'kafka-bus', essential: true, timeoutMs: 500 },
]

const MOCK_SCENARIOS: readonly ScenarioInfo[] = [
  {
    name: 'essential-outage',
    label: 'Essential dependency outage',
    description: 'Artifact Store is slowed 10x. Both Build and Test depend on it essentially.',
    expected: 'Saturation climbs into Build and Test, then into the orchestrator. Global goes FAILING.',
  },
  {
    name: 'non-essential-outage',
    label: 'Non-essential dependency outage',
    description: 'Kafka Event Bus is slowed 10x, taking Telemetry Reporter down with it.',
    expected: 'Telemetry fails, but the orchestrator treats it as optional. Global stays OK / DEGRADED.',
  },
  {
    name: 'misclassified-dependency',
    label: 'Misclassified dependency',
    description: 'SAST Engine is slowed 10x and Security Scan is reclassified as essential.',
    expected: 'An advisory scanner now takes the whole pipeline down. Global goes FAILING.',
  },
  {
    name: 'registry-slowdown',
    label: 'Registry slowdown (contained)',
    description: 'Container Registry is slowed 5x. Build depends on it non-essentially.',
    expected: 'Build degrades but keeps completing runs. The blast radius stops at Build.',
  },
  {
    name: 'cascading-failure',
    label: 'Cascading failure',
    description: 'Artifact Store slowed 10x and Build slowed 5x at the same time.',
    expected: 'Two essential hops saturate together. Everything upstream fails fast.',
  },
]

interface Injection {
  latencyMultiplier: number
  failRate: number
}

const edgeKey = (from: string, to: string): string => `${from}->${to}`

/** Non-essential dependencies cost you one severity level, not the whole run. */
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
  const essential = new Map<string, boolean>(
    EDGE_SPECS.map((e) => [edgeKey(e.from, e.to), e.essential]),
  )
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
      essential: essential.get(edgeKey(e.from, e.to)) ?? e.essential,
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

  /** Deepest tier first, so a parent always sees finished children. */
  const applyRollup = (nodes: NodeSnapshot[], edges: EdgeSnapshot[]): void => {
    const byId = new Map(nodes.map((n) => [n.id, n]))
    const ordered = [...nodes].sort((a, b) => b.tier - a.tier)
    for (const node of ordered) {
      const contributions: Status[] = [node.localStatus]
      for (const edge of edges) {
        if (edge.from !== node.id) continue
        const child = byId.get(edge.to)
        if (!child) continue
        contributions.push(edge.essential ? child.rollupStatus : demote(child.rollupStatus))
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

    return {
      atMs: startedAt + ticks * intervalMs,
      global,
      runsPerSec: rateByStatus[global] * (0.96 + random() * 0.08),
      runSuccessRate: Math.min(1, successByStatus[global] * (0.98 + random() * 0.04)),
      runP95Ms: (orchestrator?.p95LatencyMs ?? 0) + essentialP95,
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
    async setEdgeEssential(from, to, isEssential) {
      const key = edgeKey(from, to)
      if (!essential.has(key)) throw new Error(`unknown edge ${key}`)
      essential.set(key, isEssential)
      pushEvent('warn', `Edge ${from} -> ${to} reclassified as ${isEssential ? 'ESSENTIAL' : 'NON-ESSENTIAL'}`)
    },
    async applyScenario(name) {
      const scenario = MOCK_SCENARIOS.find((s) => s.name === name)
      if (!scenario) throw new Error(`unknown scenario ${name}`)
      injections.clear()
      for (const e of EDGE_SPECS) essential.set(edgeKey(e.from, e.to), e.essential)

      if (name === 'essential-outage') {
        injections.set('artifact-store', { latencyMultiplier: 10, failRate: 0 })
      } else if (name === 'non-essential-outage') {
        injections.set('kafka-bus', { latencyMultiplier: 10, failRate: 0 })
      } else if (name === 'misclassified-dependency') {
        injections.set('sast-engine', { latencyMultiplier: 10, failRate: 0 })
        essential.set(edgeKey('orchestrator', 'security-scan'), true)
      } else if (name === 'registry-slowdown') {
        injections.set('container-registry', { latencyMultiplier: 5, failRate: 0 })
      } else if (name === 'cascading-failure') {
        injections.set('artifact-store', { latencyMultiplier: 10, failRate: 0 })
        injections.set('build', { latencyMultiplier: 5, failRate: 0 })
      }
      pushEvent('warn', `Scenario applied: ${scenario.label}`)
    },
    async reset() {
      injections.clear()
      for (const e of EDGE_SPECS) essential.set(edgeKey(e.from, e.to), e.essential)
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
