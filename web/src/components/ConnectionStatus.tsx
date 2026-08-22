import { memo } from 'react'
import type { ConnectionState } from '../hooks/useSimulationStream'
import './ConnectionStatus.css'

export interface ConnectionStatusProps {
  connection: ConnectionState
  live: boolean
  stale: boolean
  lastMessageAt: number | null
  reconnectAttempt: number
  error: string | null
  /** Transport label, e.g. `SSE` or `MOCK`. */
  source: string
}

const COPY: Record<ConnectionState, { label: string; glyph: string }> = {
  connecting: { label: 'CONNECTING', glyph: '◌' },
  live: { label: 'LIVE', glyph: '●' },
  reconnecting: { label: 'RECONNECTING', glyph: '◍' },
}

/**
 * The connection indicator, and the reason the dashboard cannot lie about
 * freshness: when the stream is not live the pill says so, and `AppStaleNotice`
 * puts a banner over the data itself.
 */
function ConnectionStatusBase({
  connection,
  live,
  stale,
  reconnectAttempt,
  error,
  source,
}: ConnectionStatusProps) {
  const state: ConnectionState = live ? 'live' : connection === 'live' ? 'reconnecting' : connection
  const copy = COPY[state]

  return (
    <div className="conn" data-state={state} title={error ?? undefined}>
      <span className="conn__dot" aria-hidden="true">
        {copy.glyph}
      </span>
      <span className="conn__label mono">{copy.label}</span>
      <span className="conn__source mono">{source}</span>
      {stale && reconnectAttempt > 0 && (
        <span className="conn__attempt mono">retry #{reconnectAttempt}</span>
      )}
    </div>
  )
}

export const ConnectionStatus = memo(ConnectionStatusBase)
