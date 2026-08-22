/**
 * Transport seam between `useSimulationStream` and the outside world.
 *
 * Production uses `createSseTransport` (a real `EventSource` against
 * `/api/stream`). The dev-only mock engine implements the same interface, which
 * is what lets the whole dashboard run — and the hook be tested — without the Go
 * server. The hook owns reconnection policy; a transport only opens and closes.
 */
export interface StreamHandlers {
  onOpen: () => void
  /** Raw SSE `data` payload, still a JSON string. */
  onMessage: (data: string) => void
  onError: (message: string) => void
}

export interface StreamTransport {
  /** Opens the stream. Returns a teardown function that must be idempotent. */
  subscribe: (handlers: StreamHandlers) => () => void
  /** Shown in the connection indicator. */
  label: string
}

export const DEFAULT_STREAM_URL = '/api/stream'

export function createSseTransport(url: string = DEFAULT_STREAM_URL): StreamTransport {
  return {
    label: 'SSE',
    subscribe({ onOpen, onMessage, onError }) {
      if (typeof EventSource === 'undefined') {
        onError('EventSource is not available in this environment')
        return () => {}
      }

      const source = new EventSource(url)
      let closed = false

      const handleMessage = (event: MessageEvent<string>) => {
        onMessage(event.data)
      }

      source.onopen = () => onOpen()
      source.onmessage = handleMessage
      source.onerror = () => {
        // EventSource does not tell us why. Treat every error as "connection
        // lost" and let the hook re-open with backoff, rather than relying on
        // the browser's own opaque retry.
        onError('Stream connection lost')
      }

      // The server may label the frame instead of using the default event type.
      if (typeof source.addEventListener === 'function') {
        source.addEventListener('snapshot', handleMessage as EventListener)
      }

      return () => {
        if (closed) return
        closed = true
        source.onopen = null
        source.onmessage = null
        source.onerror = null
        if (typeof source.removeEventListener === 'function') {
          source.removeEventListener('snapshot', handleMessage as EventListener)
        }
        source.close()
      }
    },
  }
}
