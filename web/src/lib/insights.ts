import type { EdgeSnapshot, NodeSnapshot, Status } from '../types'
import { statusSeverity } from './status'

export interface DependencyInsight {
  /** Essential dependencies currently dragging this node's rollup down. */
  inheritedFrom: string[]
  /** Non-essential dependencies whose failure is being absorbed here. */
  containedFrom: string[]
}

const EMPTY: DependencyInsight = { inheritedFrom: [], containedFrom: [] }

export interface ActiveChain {
  type: 'essential_escalation' | 'contained_isolation'
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

      if (edge.essential) inheritedFrom.push(child.label)
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
): SystemDiagnostic {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const activeChains: ActiveChain[] = []

  for (const edge of edges) {
    const parent = byId.get(edge.from)
    const child = byId.get(edge.to)
    if (!parent || !child) continue

    const childUnhealthy = child.rollupStatus !== 'OK' || child.localStatus !== 'OK'
    if (!childUnhealthy) continue

    if (edge.essential && (child.rollupStatus === 'FAILING' || parent.rollupStatus === 'FAILING')) {
      activeChains.push({
        type: 'essential_escalation',
        fromLabel: parent.label,
        toLabel: child.label,
        path: `${parent.label} ──[ESSENTIAL]──► ${child.label}`,
        description: `Unbounded latency pass-through: ${parent.label} blocks on ${child.label}, propagating saturation up the DAG.`,
      })
    } else if (!edge.essential && childUnhealthy) {
      activeChains.push({
        type: 'contained_isolation',
        fromLabel: parent.label,
        toLabel: child.label,
        path: `${parent.label} ┄┄[OPTIONAL / 300ms TIMEOUT]┄┄► ${child.label}`,
        description: `Damage contained: ${parent.label} times out after 300ms, sheds ${child.label}, and continues serving.`,
      })
    }
  }

  if (status === 'FAILING') {
    return {
      status: 'FAILING',
      headline: 'Service Disruption — Escalated via Essential Dependency Path',
      mechanismTitle: 'Why the Platform is FAILING (and not DEGRADED):',
      reason:
        'A critical service failure has cascaded to the root Orchestrator along an ESSENTIAL dependency path.',
      mechanismDetail:
        'Essential dependencies pass the caller’s full 2-second request deadline. The caller blocks indefinitely on the child without a 300ms timeout cap, occupying worker goroutines and filling queues until runs fail globally.',
      contrastNote:
        'Contrast with Non-Essential: If this dependency were marked Optional, the caller would timeout after 300ms and cap the global status at DEGRADED instead.',
      activeChains,
    }
  }

  if (status === 'DEGRADED') {
    return {
      status: 'DEGRADED',
      headline: 'Degraded Performance — Damage Contained at 300ms Boundary',
      mechanismTitle: 'Why the Platform is DEGRADED (and not FAILING):',
      reason:
        'A downstream dependency is failing or saturated, but its impact is contained by a NON-ESSENTIAL classification.',
      mechanismDetail:
        'The caller invokes the child with a bounded context: context.WithTimeout(ctx, 300ms). When the child lags or fails, the caller aborts the sub-call, marks the run degraded, and continues serving. This prevents caller queue saturation and keeps global health capped at DEGRADED.',
      contrastNote:
        'Toggle Experiment: Reclassifying this edge to "Essential" in the panel will remove the 300ms cap, causing the caller to block and immediately escalating the platform from DEGRADED → FAILING under the exact same load.',
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
      'The load generator drives pipeline runs continuously at ~20 runs/sec. Essential dependencies block with full deadlines; non-essential dependencies are protected by 300ms timeout boundaries.',
    contrastNote:
      'Try injecting 10x latency into SAST Engine (optional branch) to see DEGRADED containment, or into Artifact Store (essential branch) to see a FAILING cascade.',
    activeChains,
  }
}

