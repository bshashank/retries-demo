import { memo, useCallback, useState } from 'react'
import type { EdgeSnapshot, ScenarioInfo } from '../types'
import type { SimApi } from '../lib/api'
import { formatMultiplier } from '../lib/format'
import './ControlPanel.css'

export interface NodeOption {
  id: string
  label: string
}

export interface ControlPanelProps {
  api: SimApi
  scenarios: readonly ScenarioInfo[]
  scenariosError: string | null
  edges: readonly EdgeSnapshot[]
  nodeOptions: readonly NodeOption[]
  selectedNodeId: string
  selectedMultiplier: number
  onSelectNode: (id: string) => void
}

const MULTIPLIERS = [2, 5, 10] as const

function ControlPanelBase({
  api,
  scenarios,
  scenariosError,
  edges,
  nodeOptions,
  selectedNodeId,
  selectedMultiplier,
  onSelectNode,
}: ControlPanelProps) {
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState<string | null>(null)

  const run = useCallback(async (token: string, action: () => Promise<void>) => {
    setPending(token)
    setError(null)
    try {
      await action()
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally {
      setPending(null)
    }
  }, [])

  const labelFor = (id: string): string => nodeOptions.find((n) => n.id === id)?.label ?? id

  return (
    <div className="panel controls">
      <header className="panel__head">
        <span className="panel__title">Incident controls</span>
        <button
          type="button"
          className="controls__reset"
          disabled={pending !== null}
          onClick={() => void run('reset', () => api.reset())}
        >
          Reset all
        </button>
      </header>

      <div className="controls__body">
        {error && (
          <p className="controls__error" role="alert">
            {error}
          </p>
        )}

        {/* ── Scenarios ─────────────────────────────────────────────────── */}
        <section className="controls__section">
          <h3 className="controls__heading">
            Scenarios
            <span className="controls__hint">one-click preset incidents</span>
          </h3>

          {scenariosError && (
            <p className="controls__note">Could not load scenarios — {scenariosError}</p>
          )}
          {!scenariosError && scenarios.length === 0 && (
            <p className="controls__note">No scenarios reported by the server.</p>
          )}

          <ul className="controls__scenarios">
            {scenarios.map((scenario) => (
              <li key={scenario.name}>
                <button
                  type="button"
                  className="scenario"
                  disabled={pending !== null}
                  onClick={() => void run(scenario.name, () => api.applyScenario(scenario.name))}
                >
                  <span className="scenario__label">{scenario.label || scenario.name}</span>
                  <span className="scenario__description">{scenario.description}</span>
                  <span className="scenario__expected">
                    <span className="scenario__expected-tag">EXPECT</span>
                    {scenario.expected}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </section>

        {/* ── Latency injection ─────────────────────────────────────────── */}
        <section className="controls__section">
          <h3 className="controls__heading">
            Inject latency
            <span className="controls__hint">or click a node in the graph</span>
          </h3>

          <label className="controls__field">
            <span className="visually-hidden">Target node</span>
            <select
              className="controls__select"
              value={selectedNodeId}
              onChange={(e) => onSelectNode(e.target.value)}
            >
              {nodeOptions.map((node) => (
                <option key={node.id} value={node.id}>
                  {node.label}
                </option>
              ))}
            </select>
          </label>

          <div className="controls__multipliers">
            {MULTIPLIERS.map((multiplier) => {
              const active = Math.round(selectedMultiplier) === multiplier
              return (
                <button
                  key={multiplier}
                  type="button"
                  className="multiplier"
                  data-active={active ? 'true' : undefined}
                  disabled={pending !== null}
                  onClick={() =>
                    void run(`inject-${multiplier}`, () =>
                      api.inject(selectedNodeId, multiplier, 0),
                    )
                  }
                >
                  {multiplier}x
                </button>
              )
            })}
            <button
              type="button"
              className="multiplier multiplier--recover"
              data-active={selectedMultiplier <= 1 ? 'true' : undefined}
              disabled={pending !== null}
              onClick={() => void run('recover', () => api.inject(selectedNodeId, 1, 0))}
            >
              Recover
            </button>
          </div>

          <p className="controls__status mono">
            {labelFor(selectedNodeId)} · {formatMultiplier(selectedMultiplier)} latency
          </p>
        </section>

        {/* ── Edge classification ───────────────────────────────────────── */}
        <section className="controls__section">
          <h3 className="controls__heading">
            Dependency classification
            <span className="controls__hint">flip live, mid-incident</span>
          </h3>

          <div className="controls__edge-legend">
            <div className="controls__edge-legend-item">
              <span className="controls__edge-legend-tag controls__edge-legend-tag--ess mono">ESSENTIAL (Solid)</span>
              <span>Full deadline pass-through. Child failure escalates to <strong>FAILING</strong>.</span>
            </div>
            <div className="controls__edge-legend-item">
              <span className="controls__edge-legend-tag controls__edge-legend-tag--opt mono">OPTIONAL (Dashed)</span>
              <span>300ms timeout boundary. Failure is contained at <strong>DEGRADED</strong>.</span>
            </div>
          </div>

          <ul className="controls__edges">
            {edges.map((edge) => {
              const key = `${edge.from}->${edge.to}`
              return (
                <li className="edge-row" key={key} data-essential={edge.essential ? 'true' : 'false'}>
                  <span className="edge-row__names mono" title={`timeout ${Math.round(edge.timeoutMs)}ms`}>
                    <span className="edge-row__from">{edge.from}</span>
                    <span className="edge-row__arrow" aria-hidden="true">
                      →
                    </span>
                    <span className="edge-row__to">{edge.to}</span>
                  </span>

                  <span
                    className="edge-row__toggle"
                    role="group"
                    aria-label={`${edge.from} to ${edge.to} classification`}
                  >
                    <button
                      type="button"
                      data-active={edge.essential ? 'true' : undefined}
                      disabled={pending !== null || edge.essential}
                      onClick={() =>
                        void run(`${key}-ess`, () => api.setEdgeEssential(edge.from, edge.to, true))
                      }
                    >
                      Essential
                    </button>
                    <button
                      type="button"
                      data-active={!edge.essential ? 'true' : undefined}
                      disabled={pending !== null || !edge.essential}
                      onClick={() =>
                        void run(`${key}-opt`, () => api.setEdgeEssential(edge.from, edge.to, false))
                      }
                    >
                      Optional
                    </button>
                  </span>
                </li>
              )
            })}
          </ul>
        </section>
      </div>
    </div>
  )
}

/**
 * Memoised on the values that actually change. Edge and node lists are passed in
 * already stabilised by `App`, so the whole panel sits off the 5Hz render path.
 */
export const ControlPanel = memo(ControlPanelBase)
