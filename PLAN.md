# Pipeline Health — Cascading Degradation Under Load

## Context

Anthropic Platform SWE take-home, **Theme 3: Systems & Reliability** ("infrastructure that
degrades predictably under stress"). Deliverable is a deployed prototype a reviewer can open
in a browser with zero setup, plus a GitHub repo and a design-rationale doc/video.

**The idea:** most reliability demos show a service failing. This one shows *why the same
failure produces two completely different outcomes* depending on whether the dependency was
classified essential or non-essential — and makes that classification a live toggle so the
reviewer can flip it mid-incident and watch the global status change while the underlying
physics stays identical.

**What makes it non-obvious:** degradation is *emergent*, not scripted. Services are real Go
worker pools with buffered channels as queues. Slowing a leaf doesn't set a flag — it makes
that leaf's channel fill up, which makes its essential parent block longer, which fills the
parent's channel, and so on up the DAG. Health is derived from sustained queue depth and
latency over a rolling window, so status changes reflect load genuinely piling up over ~10-20s
rather than flipping the instant you touch a control.

**Stack (per user):** Go backend (real concurrency), TypeScript/React frontend, deployed to
**Google Cloud Run**.

---

## Topology (9 nodes, 9 edges, 3 tiers)

CI/CD framing — matches the user's domain (pipelines) and the reviewed Gemini sketch.

| Edge | Essential? | Story |
|---|---|---|
| Orchestrator → Build & Compile | **yes** | no build, no pipeline |
| Orchestrator → Test Suite | **yes** | untested code doesn't ship |
| Orchestrator → Security Scan | no | scan lags → warn, still ship |
| Orchestrator → Telemetry Reporter | no | metrics are best-effort |
| Build & Compile → Artifact Store | **yes** | nowhere to put the binary |
| Build & Compile → Container Registry | no | registry push is best-effort |
| Test Suite → **Artifact Store** | **yes** | ← **shared leaf**: combined load from 2 parents |
| Security Scan → SAST Engine | **yes** | essential *to Security Scan*, which is itself non-essential |
| Telemetry Reporter → Kafka Event Bus | **yes** | Kafka lag → telemetry branch degrades |

`Artifact Store` having two essential parents is what makes this a DAG rather than a tree, and
is why scenario B escalates to DOWN: one slow leaf saturates two essential branches at once.

---

## Backend — Go (`/internal/sim`)

### Each service is a real worker pool

```go
type Service struct {
    ID, Label   string
    Tier        int
    queue       chan *Job        // buffered channel IS the queue
    workers     int
    baseLatency time.Duration
    latencyMult atomic.Int64     // injected chaos: 1x / 5x / 10x
    failRate    atomic.Uint64    // injected error rate (ppm)
    deps        []*Dep
    m           *Metrics         // rolling window, mutex-guarded
}

type Dep struct {
    Child     *Service
    Essential atomic.Bool        // runtime-toggleable from the UI
    Timeout   time.Duration      // only applied to non-essential calls
}
```

**Enqueue with load shedding** — a full queue rejects immediately rather than blocking, which
is the honest behavior and gives us a `rejected/s` metric that spikes visibly under stress:

```go
select {
case s.queue <- job:
default:
    s.m.RecordRejected()          // backpressure: queue full, shed load
    return ErrQueueFull
}
```

**Worker loop** — dequeue, record queue-wait, drop already-cancelled work (cancellation
propagation is a real behavior worth showing: work abandoned by a parent gets discarded
instead of burning capacity):

```go
for job := range s.queue {
    if job.Ctx.Err() != nil { s.m.RecordAbandoned(); continue }
    s.m.RecordQueueWait(time.Since(job.EnqueuedAt))
    s.process(job)
}
```

**process()** — own work, then fan out to children. This is where essential vs non-essential
becomes load-bearing:

```go
// own compute: sleep base*mult ± jitter, cancellable
select {
case <-time.After(s.effectiveLatency()):
case <-ctx.Done():
    return Result{Err: ctx.Err()}
}

// children, concurrently
for _, d := range s.deps {
    if d.Essential.Load() {
        // full remaining deadline; parent BLOCKS on it → child latency is additive
        r := d.Child.Call(ctx, job)
        if r.Err != nil { return Result{Err: r.Err} }   // hard fail propagates up
    } else {
        // bounded best-effort: parent gives up after Timeout and proceeds
        cctx, cancel := context.WithTimeout(ctx, d.Timeout)   // 300ms
        r := d.Child.Call(cctx, job)
        cancel()
        if r.Err != nil { result.Degraded = true }            // soft fail → DEGRADED only
    }
}
```

That asymmetry is the entire mechanism: an essential child's latency passes through
**unbounded** (so it genuinely backs up the parent's queue), while a non-essential child's cost
is **capped at 300ms** (so past that point further slowdown has *zero* effect on the parent's
queue — containment, and provably so via the parent's flat queue-depth metric).

### Load generator + health

- `LoadGen` fires runs at the Orchestrator at ~20/s (Poisson-ish), each with
  `context.WithTimeout(ctx, 2*time.Second)` as the global run deadline.
- `Metrics` = rolling ~5s window per service: p50/p95 latency, error rate, queue depth,
  rejected/s, abandoned/s.
- **Local status** thresholds off *sustained* signals (queue wait + error rate), never off the
  injected multiplier directly — this is what prevents flag-flip behavior.
- **Rollup**, computed leaves-first once per tick so the shared leaf is evaluated once:
  ```
  local==FAILING                                        -> FAILING
  any essential child rollup==FAILING                   -> FAILING
  local==DEGRADED | any essential child DEGRADED
    | any NON-essential child DEGRADED or FAILING       -> DEGRADED   (capped here)
  else                                                  -> OK
  ```
  A non-essential child can never push a parent past DEGRADED. Global banner = Orchestrator's
  rollup: `GREEN / DEGRADED / CRITICAL`.

### Tuning (starting points, verify live)

Arrival 20/s. Base latencies 10-80ms, workers 4-32 per node, queue caps 128-256, sized so every
node sits at ρ≈0.25-0.4 at 1x. `Artifact Store`: 4 workers @ 25ms → 160/s capacity vs 40/s
combined arrival; at 10x → 16/s capacity vs 40/s → queue fills in ~5s, wait exceeds the 2s run
deadline → escalation to CRITICAL in ~10-15s. `SAST Engine`: 4 workers @ 80ms; at 10x its queue
explodes but Security Scan abandons after 300ms → contained.

### API (`/internal/api`)

| Endpoint | Purpose |
|---|---|
| `GET /api/stream` | **SSE**, snapshot at 5Hz: nodes (status, queue depth, p50/p95, rejected), edges (essential flags), global status, recent events |
| `POST /api/inject` | `{nodeId, latencyMultiplier, failRate}` |
| `POST /api/edge` | `{from, to, essential}` — live reclassification |
| `POST /api/scenario` | presets: `nominal`, `noncore-slow`, `core-outage`, `kafka-lag` |
| `POST /api/reset` | drain queues, clear injections, restore default classifications |

SSE over WebSocket: one-way push is all we need, survives Cloud Run cleanly, no extra deps.

---

## Frontend — TypeScript / React (`/web`)

Vite + React + TS, plain CSS, no state library. Single `EventSource` in one hook
(`useSimulationStream`) holding the latest snapshot; components are presentational.

- `GlobalHealthBanner` — GREEN "All Systems Operational" / DEGRADED "Degraded Performance" /
  CRITICAL "Service Disruption", plus live p95.
- `DagView` — CSS grid by tier + absolutely-positioned SVG overlay for edges (measured from
  real DOM rects, so the shared leaf's two converging lines land correctly). Solid = essential,
  dashed = non-essential; stroke tinted by child status. No graph library for 9 nodes.
- `NodeCard` — label, status pill, **live queue depth** (the star metric — visibly climbing is
  the whole demo), p95, rejected/s, `10x SLOWED` badge when injected.
- `ControlPanel` — node picker + 2x/5x/10x + Recover, per-edge essential toggles, scenario
  presets, Reset.
- `EventLog` — appended on every status transition, timestamped. Makes the run self-narrating
  for a reviewer with no context (assignment requires self-contained evaluation).

---

## Deployment — Google Cloud Run

Single container: multi-stage Dockerfile (node build → go build with `embed.FS` for the static
assets → distroless). One artifact, one deploy, no CORS, no separate hosting.

```
gcloud run deploy pipeline-health --source . --region us-central1 \
  --allow-unauthenticated --max-instances 1 --min-instances 1 --no-cpu-throttling
```

Three flags matter and are worth a line in the rationale doc:
- `--max-instances 1` — one authoritative simulation; autoscaling would give different viewers
  different worlds.
- `--min-instances 1` + `--no-cpu-throttling` — Cloud Run throttles CPU between requests, which
  would freeze the background simulation. Needed so queues keep draining with no viewer attached.

Prereqs: billing enabled, Cloud Run + Cloud Build + Artifact Registry APIs on.

---

## Repo layout

```
cmd/server/main.go              # wire sim + api + embedded web, listen on $PORT
internal/sim/{graph,service,loadgen,metrics,health,scenarios}.go
internal/api/{handlers,sse}.go
web/                            # vite react-ts
Dockerfile
README.md                       # architecture + how to read the demo
```

---

## Verification

Backend first (`go run ./cmd/server`, curl the SSE stream) before any UI exists — the cascade
must be visible as raw JSON, otherwise UI work is built on unverified physics.

1. **Baseline** — 60s idle: all GREEN, queue depths ~0, no rejections. Catches over-tight thresholds.
2. **Scenario A — SAST 10x → contained.** SAST queue explodes, Security Scan goes red, banner
   goes **yellow and stays yellow**. Critically: Orchestrator's own queue depth stays flat —
   that's the proof containment is real backpressure isolation, not just display logic.
3. **Scenario B — Artifact Store 10x → cascade.** Build *and* Test both back up (shared-leaf
   combined load), banner reaches **red** in ~10-15s. Contrast with A is the headline.
4. **Live reclassification** — during A (banner yellow), toggle `Orchestrator → Security Scan`
   to essential. Banner flips **yellow → red** on the next tick with zero change to the
   underlying load. Toggle back → returns to yellow. This is the money shot for the video.
5. **Kafka lag** — 5x on Kafka Event Bus, watch queue depth climb monotonically (visible "lag"),
   Telemetry degrades, banner yellow.
6. **Reset** — from any state, queues drain, all GREEN.
7. `go vet ./...`, `npm run build`, then verify the deployed Cloud Run URL in an incognito
   window (confirms embedded assets + SSE work in the real environment, not just locally).

No automated test suite — prototype scope. Possible exception: a couple of table tests on the
rollup function, since it's pure and is the conceptual core.

---

## Scope guard

**In:** the 9-node DAG, real worker pools/queues/backpressure, essential-vs-non-essential
call semantics, live reclassification, 4 scenario presets, SSE dashboard, Cloud Run deploy.

**Out (note as future work in the rationale doc):** circuit breakers and retry/backoff on top of
the timeouts, per-node worker-count tuning from the UI, historical charts, multi-region,
persistence. Estimated ~4-5h, within the assignment's 8h cap.
