import { describe, expect, it } from 'vitest'
import type { EventLevel, Status } from '../types'
import {
  ALL_STATUSES,
  compareStatus,
  levelPresentation,
  statusDivergence,
  statusPresentation,
  statusSeverity,
  worstStatus,
} from './status'
import { computeSystemDiagnostic } from './insights'

/**
 * The status -> presentation mapping is the one place the three states are
 * defined. If any of these drift, the banner and the node pills start disagreeing
 * about what the system is doing, so the whole table is pinned.
 */
describe('statusPresentation', () => {
  const cases: Array<{
    status: Status
    headline: string
    label: string
    tone: string
    color: string
    severity: number
  }> = [
    { status: 'OK', headline: 'All Systems Operational', label: 'OK', tone: 'ok', color: '#3fb950', severity: 0 },
    { status: 'DEGRADED', headline: 'Degraded Performance', label: 'DEGRADED', tone: 'degraded', color: '#e3a008', severity: 1 },
    { status: 'FAILING', headline: 'Service Disruption', label: 'FAILING', tone: 'failing', color: '#f4544f', severity: 2 },
  ]

  it.each(cases)('maps $status to its label, colour and severity', (expected) => {
    const p = statusPresentation(expected.status)
    expect(p.status).toBe(expected.status)
    expect(p.headline).toBe(expected.headline)
    expect(p.label).toBe(expected.label)
    expect(p.tone).toBe(expected.tone)
    expect(p.color).toBe(expected.color)
    expect(p.severity).toBe(expected.severity)
  })

  it('covers every status exactly once and gives each a distinct presentation', () => {
    expect(ALL_STATUSES).toHaveLength(3)
    const tones = ALL_STATUSES.map((s) => statusPresentation(s).tone)
    const colors = ALL_STATUSES.map((s) => statusPresentation(s).color)
    const headlines = ALL_STATUSES.map((s) => statusPresentation(s).headline)
    expect(new Set(tones).size).toBe(3)
    expect(new Set(colors).size).toBe(3)
    expect(new Set(headlines).size).toBe(3)
  })

  it('pairs every status with a non-colour glyph and a text description', () => {
    for (const status of ALL_STATUSES) {
      const p = statusPresentation(status)
      expect(p.glyph.length).toBeGreaterThan(0)
      expect(p.description.length).toBeGreaterThan(0)
      expect(p.subhead.length).toBeGreaterThan(0)
    }
    // Redundant channels must actually be redundant, not the same symbol thrice.
    expect(new Set(ALL_STATUSES.map((s) => statusPresentation(s).glyph)).size).toBe(3)
  })
})

describe('severity ordering', () => {
  it('orders OK < DEGRADED < FAILING', () => {
    expect(statusSeverity('OK')).toBeLessThan(statusSeverity('DEGRADED'))
    expect(statusSeverity('DEGRADED')).toBeLessThan(statusSeverity('FAILING'))
  })

  it.each([
    ['OK', 'OK', 0],
    ['OK', 'DEGRADED', -1],
    ['DEGRADED', 'OK', 1],
    ['DEGRADED', 'FAILING', -1],
    ['FAILING', 'OK', 2],
  ] as Array<[Status, Status, number]>)('compareStatus(%s, %s)', (a, b, expected) => {
    expect(Math.sign(compareStatus(a, b))).toBe(Math.sign(expected))
  })

  it('sorts healthiest-first with compareStatus', () => {
    const sorted = (['FAILING', 'OK', 'DEGRADED', 'FAILING'] as Status[]).sort(compareStatus)
    expect(sorted).toEqual(['OK', 'DEGRADED', 'FAILING', 'FAILING'])
  })

  it.each([
    [[], 'OK'],
    [['OK', 'OK'], 'OK'],
    [['OK', 'DEGRADED'], 'DEGRADED'],
    [['DEGRADED', 'FAILING', 'OK'], 'FAILING'],
    [['FAILING'], 'FAILING'],
  ] as Array<[Status[], Status]>)('worstStatus(%j) is %s', (input, expected) => {
    expect(worstStatus(input)).toBe(expected)
  })
})

describe('statusDivergence', () => {
  it.each([
    ['OK', 'OK', 'none'],
    ['FAILING', 'FAILING', 'none'],
    // Local health is fine but an essential dependency drags the rollup down.
    ['OK', 'FAILING', 'inherited'],
    ['OK', 'DEGRADED', 'inherited'],
    // A node can never roll up healthier than it locally is, but guard it anyway.
    ['FAILING', 'OK', 'contained'],
  ] as Array<[Status, Status, string]>)(
    'local %s + rollup %s => %s',
    (local, rollup, expected) => {
      expect(statusDivergence(local, rollup)).toBe(expected)
    },
  )
})

describe('levelPresentation', () => {
  it.each([
    ['info', 'INFO'],
    ['warn', 'WARN'],
    ['critical', 'CRIT'],
  ] as Array<[EventLevel, string]>)('maps %s to %s with its own colour', (level, label) => {
    const p = levelPresentation(level)
    expect(p.label).toBe(label)
    expect(p.tone).toBe(level)
    expect(p.color).toMatch(/^#[0-9a-f]{6}$/i)
  })

  it('gives the three levels three distinct colours', () => {
    const levels: EventLevel[] = ['info', 'warn', 'critical']
    expect(new Set(levels.map((l) => levelPresentation(l).color)).size).toBe(3)
  })
})

describe('computeSystemDiagnostic', () => {
  const dummyNodes = [
    {
      id: 'orchestrator',
      label: 'Pipeline Orchestrator',
      tier: 0,
      baseLatencyMs: 10,
      localStatus: 'OK' as Status,
      rollupStatus: 'OK' as Status,
      queueDepth: 0,
      queueCapacity: 128,
      inFlight: 0,
      workers: 8,
      p50LatencyMs: 20,
      p95LatencyMs: 50,
      meanQueueWaitMs: 0,
      throughputPerSec: 20,
      errorRate: 0,
      rejectRate: 0,
      abandonRate: 0,
      latencyMultiplier: 1,
      failRate: 0,
    },
    {
      id: 'security-scan',
      label: 'Security Scan',
      tier: 1,
      baseLatencyMs: 30,
      localStatus: 'FAILING' as Status,
      rollupStatus: 'FAILING' as Status,
      queueDepth: 10,
      queueCapacity: 64,
      inFlight: 4,
      workers: 4,
      p50LatencyMs: 300,
      p95LatencyMs: 300,
      meanQueueWaitMs: 200,
      throughputPerSec: 5,
      errorRate: 1,
      rejectRate: 0,
      abandonRate: 0,
      latencyMultiplier: 10,
      failRate: 0,
    },
  ]

  it('explains OK status clearly', () => {
    const diag = computeSystemDiagnostic('OK', dummyNodes, [])
    expect(diag.status).toBe('OK')
    expect(diag.headline).toContain('Operational')
    expect(diag.reason).toContain('operating')
  })

  it('explains DEGRADED containment when edge is best-effort', () => {
    const edges = [
      { from: 'orchestrator', to: 'security-scan', mode: 'best_effort' as const, supportsGated: false, timeoutMs: 300 },
    ]
    const diag = computeSystemDiagnostic('DEGRADED', dummyNodes, edges)
    expect(diag.status).toBe('DEGRADED')
    expect(diag.mechanismDetail).toContain('300ms')
    expect(diag.activeChains).toHaveLength(1)
    expect(diag.activeChains[0].type).toBe('contained_isolation')
  })

  it('explains FAILING cascade when edge is blocking', () => {
    const edges = [
      { from: 'orchestrator', to: 'security-scan', mode: 'blocking' as const, supportsGated: false, timeoutMs: 2000 },
    ]
    const diag = computeSystemDiagnostic('FAILING', dummyNodes, edges)
    expect(diag.status).toBe('FAILING')
    expect(diag.mechanismDetail).toContain('BLOCKING')
    expect(diag.activeChains).toHaveLength(1)
    expect(diag.activeChains[0].type).toBe('essential_escalation')
    expect(diag.gatedShedNote).toBeNull()
  })

  it('explains DEGRADED as a gated hold when the backlog has headroom', () => {
    const edges = [
      { from: 'orchestrator', to: 'security-scan', mode: 'gated' as const, supportsGated: true, timeoutMs: 300 },
    ]
    const diag = computeSystemDiagnostic('DEGRADED', dummyNodes, edges)
    expect(diag.status).toBe('DEGRADED')
    expect(diag.activeChains).toHaveLength(1)
    expect(diag.activeChains[0].type).toBe('gated_hold')
    expect(diag.headline).toContain('Hold Queue')
  })

  it('notes a saturated gate can coexist with healthy release-candidate traffic', () => {
    const edges = [
      { from: 'orchestrator', to: 'security-scan', mode: 'gated' as const, supportsGated: true, timeoutMs: 300 },
    ]
    const diag = computeSystemDiagnostic('FAILING', dummyNodes, edges, 1, 0.4)
    expect(diag.status).toBe('FAILING')
    expect(diag.activeChains).toHaveLength(1)
    expect(diag.activeChains[0].type).toBe('gated_hold')
    expect(diag.gatedShedNote).not.toBeNull()
    expect(diag.gatedShedNote).toContain('100%')
    expect(diag.gatedShedNote).toContain('40%')
  })
})

