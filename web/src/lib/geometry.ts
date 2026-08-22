/**
 * Pure geometry for the DAG edge overlay.
 *
 * Everything here operates on rectangles already expressed in the SVG overlay's
 * own coordinate space (i.e. relative to the DAG container). Measuring the DOM is
 * somebody else's job — see `useNodeRects` — which keeps this math unit testable
 * and keeps layout work out of the 5Hz snapshot path.
 */

export interface Rect {
  x: number
  y: number
  width: number
  height: number
}

export interface EdgeEndpoints {
  x1: number
  y1: number
  x2: number
  y2: number
}

export interface EdgeGeometry<E> extends EdgeEndpoints {
  edge: E
  /** Cubic bezier `d` attribute. */
  path: string
  /** Midpoint of the curve, used to place the essential/non-essential marker. */
  midX: number
  midY: number
}

/**
 * Fraction of a card's width used to fan multiple anchors apart. Anchors stay
 * well inside the card so a converging pair still visibly lands on the card.
 */
const FAN_WIDTH_RATIO = 0.6

/**
 * Horizontal anchor for the `index`-th of `count` connections on a card edge.
 * A single connection anchors dead centre; multiple connections spread evenly
 * across the middle 60% of the card so converging lines stay distinguishable.
 */
export function anchorX(rect: Rect, index: number, count: number): number {
  const center = rect.x + rect.width / 2
  if (count <= 1) return center
  const usable = rect.width * FAN_WIDTH_RATIO
  const clamped = Math.min(Math.max(index, 0), count - 1)
  return center - usable / 2 + (usable * clamped) / (count - 1)
}

/**
 * Endpoints for one parent -> child edge. The parent sits in the tier above, so
 * lines leave the parent's bottom edge and arrive on the child's top edge.
 */
export function edgeEndpoints(
  from: Rect,
  to: Rect,
  fromIndex: number,
  fromCount: number,
  toIndex: number,
  toCount: number,
): EdgeEndpoints {
  return {
    x1: anchorX(from, fromIndex, fromCount),
    y1: from.y + from.height,
    x2: anchorX(to, toIndex, toCount),
    y2: to.y,
  }
}

/** Vertical bezier: leaves the parent downward, arrives at the child downward. */
export function edgePath(e: EdgeEndpoints): string {
  const dy = Math.max(Math.abs(e.y2 - e.y1) * 0.45, 12)
  return `M ${round(e.x1)} ${round(e.y1)} C ${round(e.x1)} ${round(e.y1 + dy)}, ${round(e.x2)} ${round(e.y2 - dy)}, ${round(e.x2)} ${round(e.y2)}`
}

/** Point on the cubic at t=0.5 — cheap closed form for a symmetric control net. */
export function edgeMidpoint(e: EdgeEndpoints): { midX: number; midY: number } {
  const dy = Math.max(Math.abs(e.y2 - e.y1) * 0.45, 12)
  // B(0.5) = (p0 + 3*p1 + 3*p2 + p3) / 8
  const midX = (e.x1 + 3 * e.x1 + 3 * e.x2 + e.x2) / 8
  const midY = (e.y1 + 3 * (e.y1 + dy) + 3 * (e.y2 - dy) + e.y2) / 8
  return { midX, midY }
}

function round(n: number): number {
  return Math.round(n * 100) / 100
}

export interface EdgeLike {
  from: string
  to: string
}

/**
 * Lay out every edge against a map of measured card rects.
 *
 * Anchors are fanned per endpoint so that, for example, the two essential edges
 * converging on `artifact-store` (from `build` and from `test`) land on two
 * distinct points along its top edge instead of stacking into one line. Fan order
 * follows the other endpoint's horizontal position, so lines never needlessly
 * cross. Edges whose endpoints have not been measured yet are dropped.
 */
export function layoutEdges<E extends EdgeLike>(
  edges: readonly E[],
  rects: Readonly<Record<string, Rect>>,
): EdgeGeometry<E>[] {
  const measured = edges.filter((e) => rects[e.from] !== undefined && rects[e.to] !== undefined)

  const centerX = (id: string): number => {
    const r = rects[id]
    return r.x + r.width / 2
  }

  // Outgoing anchors on each parent, ordered left-to-right by child position.
  const outgoing = new Map<string, E[]>()
  // Incoming anchors on each child, ordered left-to-right by parent position.
  const incoming = new Map<string, E[]>()

  for (const e of measured) {
    const out = outgoing.get(e.from)
    if (out) out.push(e)
    else outgoing.set(e.from, [e])

    const inc = incoming.get(e.to)
    if (inc) inc.push(e)
    else incoming.set(e.to, [e])
  }

  for (const list of outgoing.values()) {
    list.sort((a, b) => centerX(a.to) - centerX(b.to) || a.to.localeCompare(b.to))
  }
  for (const list of incoming.values()) {
    list.sort((a, b) => centerX(a.from) - centerX(b.from) || a.from.localeCompare(b.from))
  }

  return measured.map((edge) => {
    const out = outgoing.get(edge.from) as E[]
    const inc = incoming.get(edge.to) as E[]
    const endpoints = edgeEndpoints(
      rects[edge.from],
      rects[edge.to],
      out.indexOf(edge),
      out.length,
      inc.indexOf(edge),
      inc.length,
    )
    return {
      edge,
      ...endpoints,
      path: edgePath(endpoints),
      ...edgeMidpoint(endpoints),
    }
  })
}
