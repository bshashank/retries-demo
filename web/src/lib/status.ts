import type { EventLevel, Status } from '../types'

/**
 * Status -> presentation mapping. Pure, exhaustive, and the single source of truth
 * for every status label, glyph and colour in the UI. Nothing renders a status
 * string directly; everything goes through here so the three states stay
 * consistent (and so status is never conveyed by colour alone).
 */

export const ALL_STATUSES: readonly Status[] = ['OK', 'DEGRADED', 'FAILING'] as const

/** Ordering used for "worst wins" rollups. Higher is worse. */
const SEVERITY: Record<Status, number> = {
  OK: 0,
  DEGRADED: 1,
  FAILING: 2,
}

export function statusSeverity(status: Status): number {
  return SEVERITY[status]
}

/** Negative when `a` is healthier than `b`; sorts healthiest-first. */
export function compareStatus(a: Status, b: Status): number {
  return SEVERITY[a] - SEVERITY[b]
}

/** Worst status wins. Empty input is OK. */
export function worstStatus(statuses: readonly Status[]): Status {
  let worst: Status = 'OK'
  for (const s of statuses) {
    if (SEVERITY[s] > SEVERITY[worst]) worst = s
  }
  return worst
}

export interface StatusPresentation {
  status: Status
  /** Short label for pills, e.g. inside a node card. */
  label: string
  /** Full sentence used by the global banner hero. */
  headline: string
  /** Supporting line under the headline. */
  subhead: string
  /** Non-colour redundant encoding, so red/green is never the only signal. */
  glyph: string
  /** Screen-reader / title text. */
  description: string
  severity: number
  /** Resolved colour, also mirrored as a CSS custom property. */
  color: string
  cssVar: string
  /** `data-status` attribute value used by the stylesheet. */
  tone: 'ok' | 'degraded' | 'failing'
}

const PRESENTATION: Record<Status, StatusPresentation> = {
  OK: {
    status: 'OK',
    label: 'OK',
    headline: 'All Systems Operational',
    subhead: 'Every pipeline stage is inside its latency and error budget.',
    glyph: '●', // ●
    description: 'Operational',
    severity: SEVERITY.OK,
    color: '#3fb950',
    cssVar: 'var(--status-ok)',
    tone: 'ok',
  },
  DEGRADED: {
    status: 'DEGRADED',
    label: 'DEGRADED',
    headline: 'Degraded Performance',
    subhead: 'Pipeline runs are still completing, but slower or partially reduced.',
    glyph: '▲', // ▲
    description: 'Degraded',
    severity: SEVERITY.DEGRADED,
    color: '#e3a008',
    cssVar: 'var(--status-degraded)',
    tone: 'degraded',
  },
  FAILING: {
    status: 'FAILING',
    label: 'FAILING',
    headline: 'Service Disruption',
    subhead: 'Pipeline runs are failing. An essential dependency is unavailable.',
    glyph: '✖', // ✖
    description: 'Failing',
    severity: SEVERITY.FAILING,
    color: '#f4544f',
    cssVar: 'var(--status-failing)',
    tone: 'failing',
  },
}

export function statusPresentation(status: Status): StatusPresentation {
  return PRESENTATION[status]
}

export interface LevelPresentation {
  level: EventLevel
  label: string
  glyph: string
  color: string
  tone: 'info' | 'warn' | 'critical'
}

const LEVELS: Record<EventLevel, LevelPresentation> = {
  info: { level: 'info', label: 'INFO', glyph: '•', color: '#6b93c0', tone: 'info' },
  warn: { level: 'warn', label: 'WARN', glyph: '▲', color: '#e3a008', tone: 'warn' },
  critical: { level: 'critical', label: 'CRIT', glyph: '✖', color: '#f4544f', tone: 'critical' },
}

export function levelPresentation(level: EventLevel): LevelPresentation {
  return LEVELS[level]
}

/**
 * A node's headline is its rollup status, but when the rollup is worse than the
 * node's own local state the difference is the whole point of the demo: the node
 * itself is fine, it is being dragged down through an *essential* dependency.
 */
export function statusDivergence(
  localStatus: Status,
  rollupStatus: Status,
): 'none' | 'inherited' | 'contained' {
  const d = compareStatus(rollupStatus, localStatus)
  if (d === 0) return 'none'
  return d > 0 ? 'inherited' : 'contained'
}
