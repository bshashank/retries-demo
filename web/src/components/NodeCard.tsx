import { memo } from 'react'
import type { NodeSnapshot } from '../types'
import { statusPresentation } from '../lib/status'
import { formatInt, formatMs, formatMultiplier, formatPercent, formatRate, queueFillPercent } from '../lib/format'
import './NodeCard.css'

export interface NodeCardProps {
  node: NodeSnapshot
  selected?: boolean
  onSelect?: (id: string) => void
  /** Essential dependencies dragging this node down. */
  inheritedFrom?: readonly string[]
  /** Non-essential dependencies whose failure stops here. */
  containedFrom?: readonly string[]
  /** Passed through to the DAG's rect measurement. */
  cardRef?: (el: HTMLElement | null) => void
}

/**
 * One service. The headline is `rollupStatus`; `localStatus` is surfaced
 * alongside it whenever the two disagree, because that gap *is* the demo — a node
 * whose own workers are perfectly healthy can still be FAILING because an
 * essential dependency is not.
 *
 * Every row has a reserved height even when empty, so a badge appearing mid-
 * incident never changes the card's size and therefore never moves the SVG edges.
 */
function NodeCardBase({
  node,
  selected = false,
  onSelect,
  inheritedFrom,
  containedFrom,
  cardRef,
}: NodeCardProps) {
  const rollup = statusPresentation(node.rollupStatus)
  const local = statusPresentation(node.localStatus)
  const diverges = node.localStatus !== node.rollupStatus

  const fill = queueFillPercent(node.queueDepth, node.queueCapacity)
  const slowed = node.latencyMultiplier > 1
  const injectedFailures = node.failRate > 0

  const showReject = node.rejectRate > 0.0005
  const showAbandon = node.abandonRate > 0.0005

  return (
    <article
      ref={cardRef}
      className="node-card"
      data-status={rollup.tone}
      data-selected={selected ? 'true' : undefined}
      data-testid={`node-card-${node.id}`}
      aria-label={`${node.label}: ${rollup.description}`}
    >
      <button
        type="button"
        className="node-card__hit"
        onClick={() => onSelect?.(node.id)}
        aria-pressed={selected}
      >
        <span className="visually-hidden">Select {node.label} for latency injection</span>
      </button>

      <header className="node-card__head">
        <h3 className="node-card__label">{node.label}</h3>
        <span className="node-card__pill" data-status={rollup.tone}>
          <span aria-hidden="true">{rollup.glyph}</span>
          <span>{rollup.label}</span>
        </span>
      </header>

      <div className="node-card__flags">
        {slowed && (
          <span className="node-card__flag node-card__flag--slowed mono">
            {formatMultiplier(node.latencyMultiplier)} SLOWED
          </span>
        )}
        {injectedFailures && (
          <span className="node-card__flag node-card__flag--failrate mono">
            {formatPercent(node.failRate, 0)} FAIL
          </span>
        )}
        {!slowed && !injectedFailures && (
          <span className="node-card__flag node-card__flag--idle mono">nominal injection</span>
        )}
      </div>

      <div className="node-card__queue">
        <div className="node-card__queue-head">
          <span>Queue depth</span>
          <span className="mono">
            {formatInt(node.queueDepth)}
            <span className="node-card__queue-cap"> / {formatInt(node.queueCapacity)}</span>
          </span>
        </div>
        <div
          className="node-card__queue-track"
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={node.queueCapacity}
          aria-valuenow={node.queueDepth}
          aria-label={`${node.label} queue depth`}
        >
          <div
            className="node-card__queue-fill"
            data-testid={`queue-fill-${node.id}`}
            style={{ width: `${fill}%` }}
          />
        </div>
      </div>

      <dl className="node-card__metrics">
        <div>
          <dt>p95</dt>
          <dd className="mono">{formatMs(node.p95LatencyMs)}</dd>
        </div>
        <div>
          <dt>thr/s</dt>
          <dd className="mono">{formatRate(node.throughputPerSec)}</dd>
        </div>
        {showReject && (
          <div data-alert="true">
            <dt>reject</dt>
            <dd className="mono">{formatPercent(node.rejectRate, 0)}</dd>
          </div>
        )}
        {showAbandon && (
          <div data-alert="true">
            <dt>abandon</dt>
            <dd className="mono">{formatPercent(node.abandonRate, 0)}</dd>
          </div>
        )}
        {!showReject && !showAbandon && (
          <>
            <div>
              <dt>in flight</dt>
              <dd className="mono">
                {formatInt(node.inFlight)}
                <span className="node-card__queue-cap"> / {formatInt(node.workers)}</span>
              </dd>
            </div>
            <div>
              <dt>wait</dt>
              <dd className="mono">{formatMs(node.meanQueueWaitMs)}</dd>
            </div>
          </>
        )}
      </dl>

      <footer className="node-card__derivation">
        {diverges ? (
          <span className="node-card__derivation-text">
            <span className="node-card__local" data-status={local.tone}>
              local {local.label}
            </span>
            <span aria-hidden="true"> → </span>
            <span>
              rolled up {rollup.label}
              {inheritedFrom && inheritedFrom.length > 0 ? ` via ${inheritedFrom.join(', ')}` : ''}
            </span>
          </span>
        ) : containedFrom && containedFrom.length > 0 ? (
          <span className="node-card__derivation-text node-card__derivation-text--contained">
            containing {containedFrom.join(', ')}
          </span>
        ) : (
          <span className="node-card__derivation-text node-card__derivation-text--quiet">
            local and rolled-up status agree
          </span>
        )}
      </footer>
    </article>
  )
}

/**
 * Snapshots replace the whole tree at 5Hz. Compare the values the card actually
 * *renders* (already rounded to display precision) so sub-pixel metric jitter
 * does not force a re-render of every card five times a second.
 */
function displayKey(props: NodeCardProps): string {
  const n = props.node
  return [
    n.id,
    n.label,
    n.rollupStatus,
    n.localStatus,
    n.queueDepth,
    n.queueCapacity,
    n.inFlight,
    n.workers,
    Math.round(n.p95LatencyMs),
    Math.round(n.meanQueueWaitMs),
    Math.round(n.throughputPerSec * 100),
    Math.round(n.rejectRate * 1000),
    Math.round(n.abandonRate * 1000),
    Math.round(n.failRate * 1000),
    n.latencyMultiplier,
    props.selected ? 1 : 0,
    props.inheritedFrom?.join('|') ?? '',
    props.containedFrom?.join('|') ?? '',
  ].join('~')
}

export const NodeCard = memo(
  NodeCardBase,
  (prev, next) =>
    prev.onSelect === next.onSelect &&
    prev.cardRef === next.cardRef &&
    displayKey(prev) === displayKey(next),
)
