/** Display formatting helpers. Kept pure so the render path stays cheap. */

/** Queue fill as a percentage, clamped to [0, 100]. Guards a zero capacity. */
export function queueFillPercent(depth: number, capacity: number): number {
  if (!Number.isFinite(depth) || !Number.isFinite(capacity) || capacity <= 0) return 0
  if (depth <= 0) return 0
  return Math.min(100, (depth / capacity) * 100)
}

/** `10x`, `2.5x` — trims a trailing `.0`. */
export function formatMultiplier(multiplier: number): string {
  const rounded = Math.round(multiplier * 10) / 10
  return Number.isInteger(rounded) ? `${rounded}x` : `${rounded.toFixed(1)}x`
}

/** Milliseconds: sub-second stays in ms, above that switches to seconds. */
export function formatMs(ms: number): string {
  if (!Number.isFinite(ms)) return '—'
  if (ms >= 10000) return `${(ms / 1000).toFixed(1)}s`
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`
  if (ms >= 100) return `${Math.round(ms)}ms`
  return `${ms.toFixed(ms >= 10 ? 0 : 1)}ms`
}

/** A 0..1 rate as a percentage string. */
export function formatPercent(rate: number, digits = 1): string {
  if (!Number.isFinite(rate)) return '—'
  return `${(rate * 100).toFixed(digits)}%`
}

export function formatRate(perSec: number): string {
  if (!Number.isFinite(perSec)) return '—'
  if (perSec >= 100) return perSec.toFixed(0)
  if (perSec >= 10) return perSec.toFixed(1)
  return perSec.toFixed(2)
}

export function formatInt(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return Math.round(n).toLocaleString('en-US')
}

/** `14:22:07.400` — wall-clock time from an epoch-millis event timestamp. */
export function formatClock(atMs: number): string {
  const d = new Date(atMs)
  const pad = (n: number, w = 2) => String(n).padStart(w, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`
}
