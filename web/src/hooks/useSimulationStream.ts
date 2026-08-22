import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { Snapshot } from '../types'
import { createSseTransport, DEFAULT_STREAM_URL } from '../lib/transport'
import type { StreamTransport } from '../lib/transport'

export type ConnectionState = 'connecting' | 'live' | 'reconnecting'

export interface SimulationStream {
  /** Last snapshot received. Retained across a drop, but flagged `stale`. */
  snapshot: Snapshot | null
  connection: ConnectionState
  /** True only when the stream is open AND recently fed. Never true on stale data. */
  live: boolean
  /** We are holding a snapshot we can no longer vouch for. */
  stale: boolean
  /** `Date.now()` of the last successfully parsed snapshot. */
  lastMessageAt: number | null
  /** Consecutive failed connection attempts; 0 once open. */
  reconnectAttempt: number
  /** Next reconnect delay in ms while reconnecting, else null. */
  retryInMs: number | null
  error: string | null
}

export interface UseSimulationStreamOptions {
  url?: string
  /** Injected by the mock engine and by tests. Defaults to a real EventSource. */
  transport?: StreamTransport
  /** First reconnect delay; doubles per attempt. */
  baseDelayMs?: number
  maxDelayMs?: number
  /**
   * A snapshot older than this while the socket is still "open" means the server
   * stopped producing. Snapshots arrive at 5Hz, so this is a wide margin.
   */
  staleAfterMs?: number
  enabled?: boolean
}

/** Deterministic exponential backoff — no jitter, so reconnects stay testable. */
export function backoffDelay(attempt: number, baseDelayMs: number, maxDelayMs: number): number {
  const exponent = Math.max(0, attempt)
  return Math.min(maxDelayMs, baseDelayMs * 2 ** exponent)
}

interface InternalState {
  snapshot: Snapshot | null
  connection: ConnectionState
  lastMessageAt: number | null
  reconnectAttempt: number
  error: string | null
  /** Watchdog verdict: the socket looks open but nothing is arriving. */
  starved: boolean
}

const INITIAL: InternalState = {
  snapshot: null,
  connection: 'connecting',
  lastMessageAt: null,
  reconnectAttempt: 0,
  error: null,
  starved: false,
}

/**
 * Owns exactly one stream subscription and all reconnection policy.
 *
 * The important guarantee: a dropped stream never silently keeps showing the last
 * snapshot as if it were current. `live` goes false the moment the connection
 * errors *or* the feed goes quiet, and `stale` goes true whenever we are still
 * displaying data we can no longer vouch for.
 */
export function useSimulationStream(options: UseSimulationStreamOptions = {}): SimulationStream {
  const {
    url = DEFAULT_STREAM_URL,
    transport,
    baseDelayMs = 500,
    maxDelayMs = 8000,
    staleAfterMs = 2000,
    enabled = true,
  } = options

  const [state, setState] = useState<InternalState>(INITIAL)

  const defaultTransport = useMemo(() => createSseTransport(url), [url])
  const activeTransport = transport ?? defaultTransport

  // Refs mirror the pieces the watchdog and reconnect timer need to read without
  // re-subscribing the effect on every 5Hz snapshot.
  const lastMessageAtRef = useRef<number | null>(null)
  const connectionRef = useRef<ConnectionState>('connecting')

  const patch = useCallback((next: Partial<InternalState>) => {
    setState((prev) => ({ ...prev, ...next }))
  }, [])

  useEffect(() => {
    if (!enabled) return

    let disposed = false
    let unsubscribe: (() => void) | null = null
    let retryTimer: ReturnType<typeof setTimeout> | null = null
    let attempt = 0

    const teardown = () => {
      if (unsubscribe) {
        unsubscribe()
        unsubscribe = null
      }
    }

    const scheduleReconnect = (message: string) => {
      teardown()
      if (disposed) return

      const delay = backoffDelay(attempt, baseDelayMs, maxDelayMs)
      attempt += 1
      connectionRef.current = 'reconnecting'
      patch({
        connection: 'reconnecting',
        error: message,
        reconnectAttempt: attempt,
        starved: false,
      })

      retryTimer = setTimeout(() => {
        retryTimer = null
        if (!disposed) connect()
      }, delay)
    }

    const connect = () => {
      if (disposed) return
      unsubscribe = activeTransport.subscribe({
        onOpen: () => {
          if (disposed) return
          attempt = 0
          connectionRef.current = 'live'
          patch({ connection: 'live', error: null, reconnectAttempt: 0, starved: false })
        },
        onMessage: (data) => {
          if (disposed) return
          let parsed: Snapshot
          try {
            parsed = JSON.parse(data) as Snapshot
          } catch {
            // A single malformed frame is not a connection failure — keep the
            // stream open, but surface it.
            patch({ error: 'Received a malformed snapshot frame' })
            return
          }
          if (!parsed || !Array.isArray(parsed.nodes)) {
            patch({ error: 'Received a snapshot with an unexpected shape' })
            return
          }
          const now = Date.now()
          attempt = 0
          lastMessageAtRef.current = now
          connectionRef.current = 'live'
          patch({
            snapshot: parsed,
            connection: 'live',
            lastMessageAt: now,
            reconnectAttempt: 0,
            error: null,
            starved: false,
          })
        },
        onError: (message) => {
          if (disposed) return
          scheduleReconnect(message)
        },
      })
    }

    connect()

    // Watchdog: catches a half-open socket that never errors but stops producing.
    const watchdog = setInterval(() => {
      if (disposed) return
      if (connectionRef.current !== 'live') return
      const last = lastMessageAtRef.current
      const quietFor = last === null ? Number.POSITIVE_INFINITY : Date.now() - last
      const starved = quietFor > staleAfterMs
      setState((prev) => (prev.starved === starved ? prev : { ...prev, starved }))
    }, Math.max(250, Math.floor(staleAfterMs / 4)))

    return () => {
      disposed = true
      clearInterval(watchdog)
      if (retryTimer !== null) clearTimeout(retryTimer)
      teardown()
    }
  }, [activeTransport, baseDelayMs, maxDelayMs, staleAfterMs, enabled, patch])

  const live = state.connection === 'live' && !state.starved && state.lastMessageAt !== null
  const stale = state.snapshot !== null && !live

  return {
    snapshot: state.snapshot,
    connection: state.connection,
    live,
    stale,
    lastMessageAt: state.lastMessageAt,
    reconnectAttempt: state.reconnectAttempt,
    retryInMs:
      state.connection === 'reconnecting'
        ? backoffDelay(Math.max(0, state.reconnectAttempt - 1), baseDelayMs, maxDelayMs)
        : null,
    error: state.error,
  }
}
