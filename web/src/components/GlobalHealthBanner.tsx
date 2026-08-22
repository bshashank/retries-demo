import { memo } from 'react'
import type { Status } from '../types'
import { statusPresentation } from '../lib/status'
import { formatMs, formatPercent, formatRate } from '../lib/format'
import './GlobalHealthBanner.css'

export interface GlobalHealthBannerProps {
  status: Status
  runSuccessRate: number
  runsPerSec: number
  runP95Ms: number
  /** Data is being shown but the stream is not live. */
  stale?: boolean
}

/**
 * The hero. A reviewer should be able to read the outcome of an experiment from
 * this strip alone, from across the room.
 *
 * Status is carried by four redundant channels — colour, glyph, the headline
 * sentence, and the literal status token — so it never depends on telling red
 * from green.
 */
function GlobalHealthBannerBase({
  status,
  runSuccessRate,
  runsPerSec,
  runP95Ms,
  stale = false,
}: GlobalHealthBannerProps) {
  const presentation = statusPresentation(status)

  return (
    <section
      className="banner"
      data-status={presentation.tone}
      data-stale={stale ? 'true' : undefined}
      aria-live="polite"
      aria-label="Global pipeline health"
    >
      <div className="banner__signal">
        <span className="banner__glyph" aria-hidden="true">
          {presentation.glyph}
        </span>
        <div className="banner__copy">
          <div className="banner__headline-row">
            <h2 className="banner__headline">{presentation.headline}</h2>
            <span className="banner__token mono">{presentation.status}</span>
          </div>
          <p className="banner__subhead">{presentation.subhead}</p>
        </div>
      </div>

      <dl className="banner__stats">
        <div className="banner__stat">
          <dt>Run success rate</dt>
          <dd className="mono">{formatPercent(runSuccessRate)}</dd>
        </div>
        <div className="banner__stat">
          <dt>Runs / sec</dt>
          <dd className="mono">{formatRate(runsPerSec)}</dd>
        </div>
        <div className="banner__stat">
          <dt>Run p95</dt>
          <dd className="mono">{formatMs(runP95Ms)}</dd>
        </div>
      </dl>
    </section>
  )
}

export const GlobalHealthBanner = memo(GlobalHealthBannerBase)
