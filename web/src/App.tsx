import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ScenarioInfo } from './types'
import { httpApi } from './lib/api'
import { computeDependencyInsights } from './lib/insights'
import { formatClock } from './lib/format'
import { useSimulationStream } from './hooks/useSimulationStream'
import { useEventLog } from './hooks/useEventLog'
import { createMockEngine, isMockMode } from './mock/mockSnapshot'
import { GlobalHealthBanner } from './components/GlobalHealthBanner'
import { DagView } from './components/DagView'
import { ControlPanel } from './components/ControlPanel'
import { EventLog } from './components/EventLog'
import { ConnectionStatus } from './components/ConnectionStatus'

const PREFERRED_NODE = 'artifact-store'

export default function App() {
  // `?mock=1` swaps the whole data plane for a local simulation so the dashboard
  // is demoable without the Go server. Resolved once; production never touches it.
  const mock = useMemo(() => (isMockMode() ? createMockEngine() : null), [])
  const api = mock?.api ?? httpApi

  const stream = useSimulationStream({ transport: mock?.transport })
  const snapshot = stream.snapshot
  const events = useEventLog(snapshot?.events)

  const [scenarios, setScenarios] = useState<readonly ScenarioInfo[]>([])
  const [scenariosError, setScenariosError] = useState<string | null>(null)
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .scenarios()
      .then((list) => {
        if (!cancelled) {
          setScenarios(list)
          setScenariosError(null)
        }
      })
      .catch((cause: unknown) => {
        if (!cancelled) {
          setScenariosError(cause instanceof Error ? cause.message : String(cause))
        }
      })
    return () => {
      cancelled = true
    }
  }, [api])

  const nodes = snapshot?.nodes
  const edges = snapshot?.edges

  // ── Stabilised slices ───────────────────────────────────────────────────
  // Snapshots arrive at 5Hz and replace every object. These memos re-run only
  // when the value a consumer actually cares about changed, which keeps the
  // control panel and the DAG's edge layout off the per-frame render path.
  const nodeSignature = nodes?.map((n) => `${n.id}:${n.label}`).join('|') ?? ''
  const nodeOptions = useMemo(
    () => (nodes ?? []).map((n) => ({ id: n.id, label: n.label })),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed by value, not identity
    [nodeSignature],
  )

  const edgeSignature =
    edges?.map((e) => `${e.from}>${e.to}:${e.essential ? 1 : 0}:${e.timeoutMs}`).join('|') ?? ''
  const stableEdges = useMemo(
    () => edges ?? [],
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed by value, not identity
    [edgeSignature],
  )

  const insights = useMemo(
    () => (nodes && edges ? computeDependencyInsights(nodes, edges) : {}),
    [nodes, edges],
  )

  const effectiveNodeId = useMemo(() => {
    if (!nodes || nodes.length === 0) return ''
    if (selectedNodeId && nodes.some((n) => n.id === selectedNodeId)) return selectedNodeId
    return nodes.some((n) => n.id === PREFERRED_NODE) ? PREFERRED_NODE : nodes[0].id
  }, [nodes, selectedNodeId])

  const selectedMultiplier =
    nodes?.find((n) => n.id === effectiveNodeId)?.latencyMultiplier ?? 1

  const handleSelectNode = useCallback((id: string) => setSelectedNodeId(id), [])

  const source = mock ? 'MOCK' : 'SSE'

  return (
    <div className={`app${stream.stale ? ' app--stale' : ''}`}>
      <header className="app__masthead">
        <div>
          <div className="masthead__title">
            <span className="masthead__mark" aria-hidden="true" />
            Pipeline Reliability Console
            <span className="masthead__badge">{mock ? 'MOCK DATA' : 'LIVE SIMULATION'}</span>
          </div>
          <p className="masthead__blurb">
            A 9-service CI/CD pipeline running on real Go worker pools. Inject latency into any node
            and watch saturation cascade <strong>up</strong> the dependency graph. The same failure
            produces a completely different global outcome depending on whether the broken dependency
            is classified <strong>essential</strong> or <strong>non-essential</strong> — and you can
            flip that classification live, mid-incident, with the underlying load unchanged.
          </p>
        </div>
        <ConnectionStatus
          connection={stream.connection}
          live={stream.live}
          stale={stream.stale}
          lastMessageAt={stream.lastMessageAt}
          reconnectAttempt={stream.reconnectAttempt}
          error={stream.error}
          source={source}
        />
      </header>

      {stream.stale && (
        <div className="stale-notice" role="alert">
          <span className="stale-notice__glyph" aria-hidden="true">
            ⚠
          </span>
          <span>
            <strong>Stream disconnected.</strong> The values below are the last known state, not live
            data. Reconnecting…
          </span>
          {stream.lastMessageAt !== null && (
            <span className="stale-notice__detail">last update {formatClock(stream.lastMessageAt)}</span>
          )}
        </div>
      )}

      {snapshot === null ? (
        <div className="panel awaiting">
          <div className="awaiting__spinner" aria-hidden="true" />
          {stream.connection === 'reconnecting' ? (
            <>
              <span>Cannot reach the simulation stream.</span>
              <code>{stream.error ?? 'GET /api/stream'}</code>
              <code>start the Go server, or append ?mock=1 for local demo data</code>
            </>
          ) : (
            <span>Connecting to the simulation stream…</span>
          )}
        </div>
      ) : (
        <>
          <GlobalHealthBanner
            status={snapshot.global}
            runSuccessRate={snapshot.runSuccessRate}
            runsPerSec={snapshot.runsPerSec}
            runP95Ms={snapshot.runP95Ms}
            stale={stream.stale}
          />

          <div className="app__body">
            <div className="app__left">
              <DagView
                nodes={snapshot.nodes}
                edges={snapshot.edges}
                insights={insights}
                selectedNodeId={effectiveNodeId}
                onSelectNode={handleSelectNode}
              />
              <EventLog events={events} />
            </div>

            <div className="app__right">
              <ControlPanel
                api={api}
                scenarios={scenarios}
                scenariosError={scenariosError}
                edges={stableEdges}
                nodeOptions={nodeOptions}
                selectedNodeId={effectiveNodeId}
                selectedMultiplier={selectedMultiplier}
                onSelectNode={handleSelectNode}
              />
            </div>
          </div>
        </>
      )}
    </div>
  )
}
