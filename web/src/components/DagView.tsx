import { memo, useMemo } from 'react'
import type { EdgeSnapshot, NodeSnapshot, Status } from '../types'
import type { DependencyInsight } from '../lib/insights'
import { groupByTier, TIER_CAPTIONS } from '../lib/layout'
import { layoutEdges } from '../lib/geometry'
import type { EdgeLike } from '../lib/geometry'
import { statusPresentation, statusSeverity } from '../lib/status'
import { useNodeRects } from '../hooks/useNodeRects'
import { NodeCard } from './NodeCard'
import './DagView.css'

export interface DagViewProps {
  nodes: readonly NodeSnapshot[]
  edges: readonly EdgeSnapshot[]
  insights: Record<string, DependencyInsight>
  selectedNodeId: string | null
  onSelectNode: (id: string) => void
}

const edgeKey = (from: string, to: string): string => `${from}->${to}`

function DagViewBase({ nodes, edges, insights, selectedNodeId, onSelectNode }: DagViewProps) {
  const { rects, width, height, containerRef, nodeRef } = useNodeRects()

  const tiers = useMemo(() => groupByTier(nodes), [nodes])

  // Geometry depends only on measured rects and on which pairs exist — never on
  // the metrics inside a snapshot. `topology` is memoised on a string signature
  // so the 5Hz stream cannot invalidate the layout.
  const topologySignature = edges.map((e) => edgeKey(e.from, e.to)).join(',')
  const topology = useMemo<EdgeLike[]>(
    () => topologySignature.split(',').filter(Boolean).map((key) => {
      const [from, to] = key.split('->')
      return { from, to }
    }),
    [topologySignature],
  )

  const geometry = useMemo(() => layoutEdges(topology, rects), [topology, rects])

  const edgeByKey = useMemo(() => {
    const map = new Map<string, EdgeSnapshot>()
    for (const e of edges) map.set(edgeKey(e.from, e.to), e)
    return map
  }, [edges])

  const statusById = useMemo(() => {
    const map = new Map<string, Status>()
    for (const n of nodes) map.set(n.id, n.rollupStatus)
    return map
  }, [nodes])

  const nodeById = useMemo(() => {
    const map = new Map<string, NodeSnapshot>()
    for (const n of nodes) map.set(n.id, n)
    return map
  }, [nodes])

  return (
    <section className="panel dag">
      <header className="panel__head">
        <span className="panel__title">Pipeline dependency graph</span>
        <div className="dag__legend" aria-label="Edge legend">
          <span className="dag__legend-item">
            <svg width="30" height="10" aria-hidden="true">
              <line x1="1" y1="5" x2="21" y2="5" className="dag__legend-line" />
              <circle cx="25" cy="5" r="3.5" className="dag__legend-dot" />
            </svg>
            Blocking — failure propagates
          </span>
          <span className="dag__legend-item">
            <svg width="30" height="10" aria-hidden="true">
              <line
                x1="1"
                y1="5"
                x2="21"
                y2="5"
                className="dag__legend-line"
                strokeDasharray="6 2 1 2"
              />
              <circle cx="25" cy="5" r="3.5" className="dag__legend-dot" />
            </svg>
            Gated — held, then shed if saturated
          </span>
          <span className="dag__legend-item">
            <svg width="30" height="10" aria-hidden="true">
              <line
                x1="1"
                y1="5"
                x2="21"
                y2="5"
                className="dag__legend-line"
                strokeDasharray="4 4"
              />
              <circle cx="25" cy="5" r="3.5" className="dag__legend-dot dag__legend-dot--hollow" />
            </svg>
            Best effort — failure contained
          </span>
        </div>
      </header>

      <div className="dag__canvas" ref={containerRef}>
        <svg
          className="dag__edges"
          width={width || undefined}
          height={height || undefined}
          viewBox={width && height ? `0 0 ${width} ${height}` : undefined}
          aria-hidden="true"
          focusable="false"
        >
          <defs>
            {(['ok', 'degraded', 'failing'] as const).map((tone) => (
              <marker
                key={tone}
                id={`dag-arrow-${tone}`}
                viewBox="0 0 8 8"
                refX="6.4"
                refY="4"
                markerWidth="5"
                markerHeight="5"
                orient="auto-start-reverse"
              >
                <path d="M 0 1 L 7 4 L 0 7 z" className={`dag__arrow dag__arrow--${tone}`} />
              </marker>
            ))}
          </defs>

          {geometry.map((g) => {
            const key = edgeKey(g.edge.from, g.edge.to)
            const edge = edgeByKey.get(key)
            if (!edge) return null

            const childStatus = statusById.get(edge.to) ?? 'OK'
            const parentStatus = statusById.get(edge.from) ?? 'OK'
            const tone = statusPresentation(childStatus).tone

            // Only annotate edges that are actually doing something right now, so
            // a healthy graph stays clean and an incident is self-explanatory.
            const childIsHurting = childStatus !== 'OK'
            const contained =
              childIsHurting &&
              edge.mode === 'best_effort' &&
              statusSeverity(childStatus) > statusSeverity(parentStatus)
            const propagating =
              childIsHurting &&
              edge.mode === 'blocking' &&
              statusSeverity(parentStatus) >= statusSeverity(childStatus)
            const held = childIsHurting && edge.mode === 'gated'
            const annotation = contained
              ? 'CONTAINED'
              : propagating
                ? 'PROPAGATING'
                : held
                  ? 'HELD'
                  : null
            const annotationTag = contained ? 'contained' : propagating ? 'propagating' : 'held'
            const annotationWidth = annotation ? annotation.length * 5.4 + 12 : 0
            const modeClass =
              edge.mode === 'gated'
                ? 'dag__edge--gated'
                : edge.mode === 'blocking'
                  ? 'dag__edge--essential'
                  : 'dag__edge--optional'
            const markerClass = edge.mode === 'best_effort' ? 'dag__edge-marker--optional' : 'dag__edge-marker--essential'
            const modeDescription =
              edge.mode === 'gated' ? 'gated' : edge.mode === 'blocking' ? 'blocking' : 'best-effort'

            return (
              <g key={key} data-status={tone}>
                <path
                  d={g.path}
                  className={`dag__edge ${modeClass}`}
                  markerEnd={`url(#dag-arrow-${tone})`}
                />
                <circle
                  cx={g.midX}
                  cy={g.midY}
                  r={4}
                  className={`dag__edge-marker ${markerClass}`}
                />
                {annotation && (
                  <g className={`dag__edge-tag dag__edge-tag--${annotationTag}`}>
                    <rect
                      x={g.midX + 8}
                      y={g.midY - 7.5}
                      width={annotationWidth}
                      height={15}
                      rx={3}
                    />
                    <text x={g.midX + 8 + annotationWidth / 2} y={g.midY + 3.6}>
                      {annotation}
                    </text>
                  </g>
                )}
                <title>
                  {`${nodeById.get(edge.from)?.label ?? edge.from} depends on ${
                    nodeById.get(edge.to)?.label ?? edge.to
                  } — ${modeDescription}, timeout ${Math.round(edge.timeoutMs)}ms`}
                </title>
              </g>
            )
          })}
        </svg>

        <div className="dag__tiers">
          {tiers.map((tier) => (
            <div className="dag__tier" key={tier.tier}>
              <div className="dag__tier-caption">{TIER_CAPTIONS[tier.tier] ?? `Tier ${tier.tier}`}</div>
              <div
                className="dag__row"
                data-single={tier.nodes.length === 1 ? 'true' : undefined}
              >
                {tier.nodes.map((node) => (
                  <NodeCard
                    key={node.id}
                    node={node}
                    cardRef={nodeRef(node.id)}
                    selected={node.id === selectedNodeId}
                    onSelect={onSelectNode}
                    inheritedFrom={insights[node.id]?.inheritedFrom}
                    containedFrom={insights[node.id]?.containedFrom}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  )
}

export const DagView = memo(DagViewBase)
