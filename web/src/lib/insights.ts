import type { EdgeSnapshot, NodeSnapshot } from '../types'
import { statusSeverity } from './status'

export interface DependencyInsight {
  /** Essential dependencies currently dragging this node's rollup down. */
  inheritedFrom: string[]
  /** Non-essential dependencies whose failure is being absorbed here. */
  containedFrom: string[]
}

const EMPTY: DependencyInsight = { inheritedFrom: [], containedFrom: [] }

/**
 * Works out, per node, which dependencies are propagating damage and which are
 * having their damage contained. This is the sentence the demo is trying to make
 * a reviewer read off the screen: "Build is failing *because of* Artifact Store"
 * versus "the orchestrator is fine *despite* Telemetry".
 *
 * Cheap enough (9 nodes, 9 edges) to run per snapshot.
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
