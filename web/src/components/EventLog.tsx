import { memo } from 'react'
import type { SimEvent } from '../types'
import { levelPresentation } from '../lib/status'
import { formatClock } from '../lib/format'
import { EVENT_LOG_LIMIT } from '../hooks/useEventLog'
import './EventLog.css'

export interface EventLogProps {
  events: readonly SimEvent[]
}

function EventLogBase({ events }: EventLogProps) {
  return (
    <section className="panel event-log">
      <header className="panel__head">
        <span className="panel__title">Event log</span>
        <span className="event-log__meta mono">
          newest first · {events.length}/{EVENT_LOG_LIMIT}
        </span>
      </header>

      <ol className="event-log__list">
        {events.length === 0 && (
          <li className="event-log__empty">
            No transitions yet. Inject latency into a node to start an incident.
          </li>
        )}
        {events.map((event) => {
          const level = levelPresentation(event.level)
          return (
            <li className="event-log__row" key={event.id} data-level={level.tone}>
              <span className="event-log__time mono">{formatClock(event.atMs)}</span>
              <span className="event-log__level mono">
                <span aria-hidden="true">{level.glyph}</span> {level.label}
              </span>
              <span className="event-log__message">{event.message}</span>
            </li>
          )
        })}
      </ol>
    </section>
  )
}

export const EventLog = memo(EventLogBase)
