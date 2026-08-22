import { memo, useState } from 'react'
import type { SystemDiagnostic } from '../lib/insights'
import { statusPresentation } from '../lib/status'
import './CausalDiagnosticCard.css'

export interface CausalDiagnosticCardProps {
  diagnostic: SystemDiagnostic
}

function CausalDiagnosticCardBase({ diagnostic }: CausalDiagnosticCardProps) {
  const presentation = statusPresentation(diagnostic.status)
  const [expanded, setExpanded] = useState(true)

  return (
    <div className="diagnostic-card" data-status={presentation.tone}>
      <header className="diagnostic-card__header">
        <div className="diagnostic-card__title-row">
          <span className="diagnostic-card__pill" data-status={presentation.tone}>
            <span aria-hidden="true">{presentation.glyph}</span>
            <span>{presentation.label} DIAGNOSTIC</span>
          </span>
          <h3 className="diagnostic-card__headline">{diagnostic.headline}</h3>
        </div>
        <button
          type="button"
          className="diagnostic-card__toggle-btn"
          onClick={() => setExpanded((prev) => !prev)}
          aria-expanded={expanded}
        >
          {expanded ? 'Hide Explanation ▲' : 'Show Why This Happened ▼'}
        </button>
      </header>

      {expanded && (
        <div className="diagnostic-card__body">
          <div className="diagnostic-card__section">
            <h4 className="diagnostic-card__section-title">{diagnostic.mechanismTitle}</h4>
            <p className="diagnostic-card__reason">{diagnostic.reason}</p>
            <p className="diagnostic-card__detail mono">{diagnostic.mechanismDetail}</p>
          </div>

          {diagnostic.activeChains.length > 0 && (
            <div className="diagnostic-card__chains">
              <h4 className="diagnostic-card__section-title">Active Failure & Propagation Path:</h4>
              <ul className="diagnostic-card__chain-list">
                {diagnostic.activeChains.map((chain, index) => (
                  <li
                    key={`${chain.path}-${index}`}
                    className="diagnostic-card__chain-item"
                    data-chain-type={chain.type}
                  >
                    <div className="diagnostic-card__chain-header">
                      <span className="diagnostic-card__chain-badge mono">
                        {chain.type === 'essential_escalation'
                          ? '🔴 ESSENTIAL CASCADE'
                          : '🟡 300ms TIMEOUT ISOLATION'}
                      </span>
                      <strong className="diagnostic-card__chain-path mono">{chain.path}</strong>
                    </div>
                    <p className="diagnostic-card__chain-desc">{chain.description}</p>
                  </li>
                ))}
              </ul>
            </div>
          )}

          <div className="diagnostic-card__contrast">
            <span className="diagnostic-card__contrast-tag">DEGRADED vs. FAILING RULE</span>
            <p className="diagnostic-card__contrast-text">{diagnostic.contrastNote}</p>
          </div>
        </div>
      )}
    </div>
  )
}

export const CausalDiagnosticCard = memo(CausalDiagnosticCardBase)
