import type { NodeSnapshot } from '../types'

/**
 * Presentation-only layout hints. The topology, labels and edges all come from
 * the server; this file only decides the left-to-right order within each tier so
 * that dependency lines fan out without crossing. Unknown ids fall back to the
 * server's own ordering, so a topology change degrades gracefully rather than
 * dropping nodes.
 */
const TIER_ORDER: Readonly<Record<number, readonly string[]>> = {
  1: ['orchestrator'],
  2: ['build', 'test', 'security-scan', 'telemetry'],
  3: ['container-registry', 'artifact-store', 'sast-engine', 'kafka-bus'],
}

export interface Tier {
  tier: number
  nodes: NodeSnapshot[]
}

/** Group nodes into ascending tiers, ordered within each tier by the hints above. */
export function groupByTier(nodes: readonly NodeSnapshot[]): Tier[] {
  const byTier = new Map<number, NodeSnapshot[]>()
  for (const node of nodes) {
    const list = byTier.get(node.tier)
    if (list) list.push(node)
    else byTier.set(node.tier, [node])
  }

  return [...byTier.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([tier, tierNodes]) => {
      const hint = TIER_ORDER[tier] ?? []
      const rank = (id: string): number => {
        const i = hint.indexOf(id)
        return i === -1 ? Number.MAX_SAFE_INTEGER : i
      }
      return {
        tier,
        nodes: [...tierNodes].sort(
          (a, b) => rank(a.id) - rank(b.id) || tierNodes.indexOf(a) - tierNodes.indexOf(b),
        ),
      }
    })
}

export const TIER_CAPTIONS: Readonly<Record<number, string>> = {
  1: 'Tier 1 · entry point',
  2: 'Tier 2 · pipeline stages',
  3: 'Tier 3 · backing services',
}
