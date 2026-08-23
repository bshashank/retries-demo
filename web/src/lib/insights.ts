import { isEssential, type EdgeSnapshot, type NodeSnapshot, type Status } from '../types'
import { statusSeverity } from './status'

export interface DependencyInsight {
  /** Essential (blocking or gated) dependencies currently dragging this node's rollup down. */
  inheritedFrom: string[]
  /** Best-effort dependencies whose failure is being absorbed here. */
  containedFrom: string[]
}

const EMPTY: DependencyInsight = { inheritedFrom: [], containedFrom: [] }

export interface ActiveChain {
  type: 'essential_escalation' | 'contained_isolation' | 'gated_hold'
  fromLabel: string
  toLabel: string
  path: string
  description: string
}

export interface SystemDiagnostic {
  status: Status
  headline: string
  mechanismTitle: string
  reason: string
  mechanismDetail: string
  contrastNote: string
  /** Set only when a gated dependency's saturation is part of why status is FAILING. */
  gatedShedNote: string | null
  activeChains: ActiveChain[]
}

/**
 * Works out, per node, which dependencies are propagating damage and which are
 * having their damage contained.
 */
export function computeDependencyInsights(
  nodes: readonly NodeSnapshot[],
  edges: readonly EdgeSnapshot[],
): Record<string, DependencyInsight> {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const result: Record<string, DependencyInsight> = {}

  for (const node of nodes) {
    const inheritedFrom: string[] = []
    const containedFrom: string[] = []

    for (const edge of edges) {
      if (edge.from !== node.id) continue
      const child = byId.get(edge.to)
      if (!child) continue
      if (statusSeverity(child.rollupStatus) <= statusSeverity(node.localStatus)) continue

      if (isEssential(edge.mode)) inheritedFrom.push(child.label)
      else containedFrom.push(child.label)
    }

    result[node.id] =
      inheritedFrom.length === 0 && containedFrom.length === 0
        ? EMPTY
        : { inheritedFrom, containedFrom }
  }

  return result
}

/**
 * Derives explicit causal explanations for why the platform is currently
 * OK, DEGRADED, or FAILING, highlighting the difference in failure propagation
 * between essential and non-essential dependencies.
 */
export function computeSystemDiagnostic(
  status: Status,
  nodes: readonly NodeSnapshot[],
  edges: readonly EdgeSnapshot[],
  runSuccessRateRC?: number,
  runSuccessRateNormal?: number,
): SystemDiagnostic {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const activeChains: ActiveChain[] = []

  for (const edge of edges) {
    const parent = byId.get(edge.from)
    const child = byId.get(edge.to)
    if (!parent || !child) continue

    const childUnhealthy = child.rollupStatus !== 'OK' || child.localStatus !== 'OK'
    if (!childUnhealthy) continue

    if (edge.mode === 'gated') {
      activeChains.push({
        type: 'gated_hold',
        fromLabel: parent.label,
        toLabel: child.label,
        path: `${parent.label} ┈┈[GATED / HOLD QUEUE]┈┈► ${child.label}`,
        description: `Bounded backlog, not a block or a skip: ${parent.label} holds the call in ${child.label}'s queue (up to a long grace budget) instead of blocking its own workers or silently proceeding. Only Normal-priority runs are shed if that backlog saturates — release-candidate runs never are.`,
      })
    } else if (
      edge.mode === 'blocking' &&
      (child.rollupStatus === 'FAILING' || parent.rollupStatus === 'FAILING')
    ) {
      activeChains.push({
        type: 'essential_escalation',
        fromLabel: parent.label,
        toLabel: child.label,
        path: `${parent.label} ──[BLOCKING]──► ${child.label}`,
        description: `Unbounded latency pass-through: ${parent.label} blocks on ${child.label}, propagating saturation up the DAG.`,
      })
    } else if (edge.mode === 'best_effort') {
      activeChains.push({
        type: 'contained_isolation',
        fromLabel: parent.label,
        toLabel: child.label,
        path: `${parent.label} ┄┄[BEST EFFORT / 300ms TIMEOUT]┄┄► ${child.label}`,
        description: `Damage contained: ${parent.label} times out after 300ms, sheds ${child.label}, and continues serving.`,
      })
    }
  }

  const gatedChains = activeChains.filter((c) => c.type === 'gated_hold')
  const gatedShedNote =
    status === 'FAILING' &&
    gatedChains.length > 0 &&
    runSuccessRateRC !== undefined &&
    runSuccessRateNormal !== undefined
      ? `This FAILING reading can coexist with healthy release traffic: it means a gate (${gatedChains
          .map((c) => c.toLabel)
          .join(', ')}) is saturated and shedding Normal-priority runs, not that every run is failing. ` +
        `Right now release-candidate success is ${(runSuccessRateRC * 100).toFixed(0)}% versus ${(
          runSuccessRateNormal * 100
        ).toFixed(0)}% for Normal-priority runs.`
      : null

  if (status === 'FAILING') {
    return {
      status: 'FAILING',
      headline: 'Service Disruption — Escalated via Essential Dependency Path',
      mechanismTitle: 'Why the Platform is FAILING (and not DEGRADED):',
      reason:
        'A critical service failure has cascaded to the root Orchestrator along an essential dependency path — either a BLOCKING call with no timeout, or a GATED hold queue that has run out of headroom.',
      mechanismDetail:
        'BLOCKING dependencies pass the caller’s full 2-second request deadline: the caller blocks indefinitely on the child without a timeout cap, occupying worker goroutines and filling queues until runs fail globally. A GATED dependency instead holds the call in the child’s own bounded backlog — the caller’s queue stays flat — and only escalates to FAILING once that backlog itself saturates and starts shedding Normal-priority runs.',
      contrastNote:
        'Contrast with Best Effort: if this dependency were marked Best Effort, the caller would timeout after 300ms and cap the global status at DEGRADED instead.',
      gatedShedNote,
      activeChains,
    }
  }

  if (status === 'DEGRADED') {
    const hasGated = gatedChains.length > 0
    return {
      status: 'DEGRADED',
      headline: hasGated
        ? 'Degraded Performance — Backlog Building in a Hold Queue'
        : 'Degraded Performance — Damage Contained at 300ms Boundary',
      mechanismTitle: 'Why the Platform is DEGRADED (and not FAILING):',
      reason: hasGated
        ? 'A gated dependency is slow, and calls are piling up in its bounded backlog — but the backlog still has headroom, so nothing is being shed yet.'
        : 'A downstream dependency is failing or saturated, but its impact is contained by a BEST EFFORT classification.',
      mechanismDetail: hasGated
        ? 'The caller resolves fast for the grace window, then hands the call off to a detached background attempt against the gated resource’s own queue instead of blocking a worker or giving up. The caller’s own queue and latency stay flat while the backlog absorbs the slowdown.'
        : 'The caller invokes the child with a bounded context: context.WithTimeout(ctx, 300ms). When the child lags or fails, the caller aborts the sub-call, marks the run degraded, and continues serving. This prevents caller queue saturation and keeps global health capped at DEGRADED.',
      contrastNote: hasGated
        ? 'Toggle Experiment: Reclassifying this edge to "Blocking" in the panel removes the hold queue entirely, letting the slowdown propagate straight into the caller’s own queue and escalating to FAILING immediately under the same load.'
        : 'Toggle Experiment: Reclassifying this edge to "Blocking" in the panel will remove the 300ms cap, causing the caller to block and immediately escalating the platform from DEGRADED → FAILING under the exact same load.',
      gatedShedNote: null,
      activeChains,
    }
  }

  return {
    status: 'OK',
    headline: 'All Systems Operational — Workloads Nominal',
    mechanismTitle: 'How Degradation Rules Apply:',
    reason:
      'All 9 worker pools are operating within baseline queue capacity, latency budgets, and error rates.',
    mechanismDetail:
      'The load generator drives pipeline runs continuously at ~20 runs/sec. Blocking dependencies pass through with full deadlines; best-effort dependencies are protected by 300ms timeout boundaries; gated dependencies (SAST, Container Registry) absorb slowness into a bounded backlog before shedding only Normal-priority traffic.',
    contrastNote:
      'Try the SAST Engine Slowdown or Container Registry Slowdown scenarios to see the gated hold-then-shed pattern, or Artifact Store Outage to see a blocking cascade.',
    gatedShedNote: null,
    activeChains,
  }
}

