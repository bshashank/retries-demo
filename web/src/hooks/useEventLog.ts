import { useEffect, useRef, useState } from 'react'
import type { SimEvent } from '../types'

export const EVENT_LOG_LIMIT = 100

/**
 * Accumulates the event feed, newest first, capped at `EVENT_LOG_LIMIT`.
 *
 * Snapshots carry an `events` array. Whether the server sends the full history or
 * only a recent window is not something the log should depend on, so events are
 * merged by id and de-duplicated — both server behaviours produce the same log.
 * The returned array identity only changes when something new actually arrived,
 * which keeps the memoised `EventLog` off the 5Hz render path.
 */
export function useEventLog(incoming: readonly SimEvent[] | undefined): SimEvent[] {
  const [events, setEvents] = useState<SimEvent[]>([])
  const seenRef = useRef(new Set<number>())
  const highWaterRef = useRef(-1)

  useEffect(() => {
    if (!incoming || incoming.length === 0) return

    // A server restart (or a fresh engine) rewinds event ids. Drop the history
    // rather than silently swallowing everything that follows.
    const highest = incoming.reduce((max, e) => (e.id > max ? e.id : max), -1)
    if (highest < highWaterRef.current) {
      seenRef.current = new Set()
      highWaterRef.current = -1
      setEvents([])
    }
    highWaterRef.current = Math.max(highWaterRef.current, highest)

    const fresh = incoming.filter((e) => !seenRef.current.has(e.id))
    if (fresh.length === 0) return
    for (const e of fresh) seenRef.current.add(e.id)

    setEvents((prev) => {
      const merged = [...fresh, ...prev].sort((a, b) => b.id - a.id).slice(0, EVENT_LOG_LIMIT)
      // Keep the seen-set from growing without bound across a long session.
      if (seenRef.current.size > EVENT_LOG_LIMIT * 8) {
        seenRef.current = new Set(merged.map((e) => e.id))
      }
      return merged
    })
  }, [incoming])

  return events
}
