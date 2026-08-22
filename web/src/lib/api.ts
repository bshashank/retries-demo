import type { ScenarioInfo } from '../types'

/**
 * Control surface. Mirrors the `sim.Controller` interface exposed over HTTP.
 * The dev mock engine implements the same `SimApi` shape, so no component ever
 * has to know which one it is talking to.
 */
export interface SimApi {
  /** POST /api/inject */
  inject: (nodeId: string, latencyMultiplier: number, failRate: number) => Promise<void>
  /** POST /api/edge */
  setEdgeEssential: (from: string, to: string, essential: boolean) => Promise<void>
  /** POST /api/scenario */
  applyScenario: (name: string) => Promise<void>
  /** POST /api/reset */
  reset: () => Promise<void>
  /** GET /api/scenarios */
  scenarios: () => Promise<ScenarioInfo[]>
}

async function postJson(path: string, body?: unknown): Promise<void> {
  const response = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!response.ok) {
    const detail = await response.text().catch(() => '')
    throw new Error(`${path} failed: ${response.status}${detail ? ` ${detail.slice(0, 200)}` : ''}`)
  }
}

export const httpApi: SimApi = {
  inject: (nodeId, latencyMultiplier, failRate) =>
    postJson('/api/inject', { nodeId, latencyMultiplier, failRate }),

  setEdgeEssential: (from, to, essential) => postJson('/api/edge', { from, to, essential }),

  applyScenario: (name) => postJson('/api/scenario', { name }),

  reset: () => postJson('/api/reset'),

  scenarios: async () => {
    const response = await fetch('/api/scenarios')
    if (!response.ok) throw new Error(`/api/scenarios failed: ${response.status}`)
    const data: unknown = await response.json()
    return Array.isArray(data) ? (data as ScenarioInfo[]) : []
  },
}
