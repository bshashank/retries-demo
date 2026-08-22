import { useCallback, useEffect, useRef, useState } from 'react'
import type { Rect } from '../lib/geometry'

export interface MeasuredLayout {
  rects: Record<string, Rect>
  width: number
  height: number
}

export interface NodeRects extends MeasuredLayout {
  containerRef: (el: HTMLDivElement | null) => void
  /** Stable per id — safe to pass straight to `ref` without re-triggering. */
  nodeRef: (id: string) => (el: HTMLElement | null) => void
}

const EMPTY: MeasuredLayout = { rects: {}, width: 0, height: 0 }

/**
 * Measures node cards relative to their container so the SVG edge overlay can
 * draw between real DOM positions.
 *
 * Measurement is driven by mount and by `ResizeObserver` only — never by the
 * snapshot stream. Snapshots arrive at 5Hz and must not cause a single
 * `getBoundingClientRect` call. The per-id ref callbacks are cached so a
 * re-render does not detach/reattach refs and re-trigger measurement.
 */
export function useNodeRects(): NodeRects {
  const [layout, setLayout] = useState<MeasuredLayout>(EMPTY)

  const containerElRef = useRef<HTMLDivElement | null>(null)
  const elementsRef = useRef(new Map<string, HTMLElement>())
  const refCacheRef = useRef(new Map<string, (el: HTMLElement | null) => void>())
  const observerRef = useRef<ResizeObserver | null>(null)
  const frameRef = useRef<number | null>(null)
  const signatureRef = useRef('')

  const measure = useCallback(() => {
    frameRef.current = null
    const container = containerElRef.current
    if (!container) return

    const base = container.getBoundingClientRect()
    const rects: Record<string, Rect> = {}
    for (const [id, el] of elementsRef.current) {
      const r = el.getBoundingClientRect()
      rects[id] = {
        x: r.left - base.left,
        y: r.top - base.top,
        width: r.width,
        height: r.height,
      }
    }

    const next: MeasuredLayout = { rects, width: base.width, height: base.height }
    const signature = JSON.stringify(next)
    if (signature === signatureRef.current) return
    signatureRef.current = signature
    setLayout(next)
  }, [])

  const scheduleMeasure = useCallback(() => {
    if (frameRef.current !== null) return
    if (typeof requestAnimationFrame === 'function') {
      frameRef.current = requestAnimationFrame(measure)
    } else {
      measure()
    }
  }, [measure])

  const observe = useCallback((el: Element) => {
    observerRef.current?.observe(el)
  }, [])

  const unobserve = useCallback((el: Element) => {
    observerRef.current?.unobserve(el)
  }, [])

  const containerRef = useCallback(
    (el: HTMLDivElement | null) => {
      const previous = containerElRef.current
      if (previous && previous !== el) unobserve(previous)
      containerElRef.current = el
      if (el) {
        observe(el)
        scheduleMeasure()
      }
    },
    [observe, unobserve, scheduleMeasure],
  )

  const nodeRef = useCallback(
    (id: string) => {
      const cached = refCacheRef.current.get(id)
      if (cached) return cached
      const fn = (el: HTMLElement | null) => {
        const previous = elementsRef.current.get(id)
        if (previous && previous !== el) unobserve(previous)
        if (el) {
          elementsRef.current.set(id, el)
          observe(el)
        } else {
          elementsRef.current.delete(id)
        }
        scheduleMeasure()
      }
      refCacheRef.current.set(id, fn)
      return fn
    },
    [observe, unobserve, scheduleMeasure],
  )

  useEffect(() => {
    if (typeof ResizeObserver !== 'undefined') {
      const observer = new ResizeObserver(() => scheduleMeasure())
      observerRef.current = observer
      if (containerElRef.current) observer.observe(containerElRef.current)
      for (const el of elementsRef.current.values()) observer.observe(el)
    }

    const onResize = () => scheduleMeasure()
    window.addEventListener('resize', onResize)
    scheduleMeasure()

    return () => {
      window.removeEventListener('resize', onResize)
      observerRef.current?.disconnect()
      observerRef.current = null
      if (frameRef.current !== null && typeof cancelAnimationFrame === 'function') {
        cancelAnimationFrame(frameRef.current)
      }
      frameRef.current = null
    }
  }, [scheduleMeasure])

  return { ...layout, containerRef, nodeRef }
}
