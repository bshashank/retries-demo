# Pipeline Health — Cascading Degradation Under Load

A live dashboard of a 9-node CI/CD pipeline where **every service is a real Go worker pool**.
Slow down one leaf node and watch the degradation propagate up the dependency graph in real
time — or fail to propagate, depending on how the dependency was classified.

**Live demo:** https://pipeline-health-oifjodl6rq-uc.a.run.app (see [DEPLOY.md](DEPLOY.md))

---

## The idea in one paragraph

Most reliability demos show a service failing. This one shows why **the same failure produces
different global outcomes** depending on how the broken dependency is classified — and lets you
flip that classification live, mid-incident, while the underlying load stays identical. There are
three classifications, not two: a dependency can **block** (cascades), run **best-effort**
(contained at a timeout), or be **gated** — essential for correctness, but held in a bounded
backlog instead of blocking a worker or being silently skipped, and shed only for non-critical
traffic once that backlog itself saturates. Watching a red node leave the global banner green —
or watching release-candidate traffic keep succeeding while the banner reads FAILING — is the
moment the concept lands.

## Why it isn't a scripted animation

Nothing here is a state machine with hardcoded transitions. Each of the 9 services is a real
worker pool: a **buffered Go channel is the queue**, N goroutines are the workers, and a load
generator drives ~20 pipeline runs/sec through the DAG continuously.

When you slow a leaf down 10x:

1. That node's workers take longer per job, so its capacity drops below its arrival rate.
2. Its buffered channel — a real queue — starts filling. Queue wait time climbs.
3. What happens next depends on the edge's mode. A **blocking** parent calls it with the full
   remaining request deadline, so the parent's workers stay occupied longer too, and the
   *parent's* capacity drops — the cascade climbs one tier at a time. A **gated** parent instead
   hands the call to the child's own backlog after a short grace window and moves on, so the
   parent's queue never even feels it.
4. Once a queue hits capacity, enqueues are shed rather than blocked — a visible `rejected/s`.
   For a gated node this is priority-aware: release-candidate traffic is never shed, only Normal.

Health is derived from *sustained* signals (mean queue wait, error/reject/abandon rates over a
5-second rolling window), never from the injected multiplier directly. That is deliberate: it
means status changes lag the injection by seconds, the way real incidents do, instead of
flipping the instant you touch a control.

## Three dependency modes, not two

Early on this was a plain essential/non-essential boolean. It broke on a real case: a security
scan and a container registry push are both essential — a pipeline that can't get a SAST result
or land its image can't ship — but neither should be able to block a worker indefinitely the way
a classic essential dependency does. "Essential or not" conflates two separate questions: *does a
failure here propagate* (correctness) and *does this call block the caller* (dispatch). A
dependency mode answers both at once:

| | `blocking` | `best_effort` | `gated` |
|---|---|---|---|
| Context | caller's full remaining deadline | `context.WithTimeout(ctx, 300ms)` | short grace window, then a detached background attempt bounded by a long hold budget |
| Parent behaviour | **blocks** until it answers | gives up at 300ms, proceeds anyway | resolves fast if healthy; if slow, hands the call to the child's own backlog and moves on |
| Child latency cost to parent | **unbounded** — fully additive | **capped at 300ms** | **capped at the grace window** — the backlog absorbs the rest |
| On sustained failure | parent fails, propagates up | parent sets `degraded`, keeps serving | backlog fills; once it saturates, Normal-priority runs are shed, release-candidate runs never are |
| Essential? (derived) | yes | no | yes — `modeEssential(mode) != best_effort` |
| Worst global outcome | `FAILING` | `DEGRADED` — capped, structurally | `DEGRADED` while the backlog has headroom, `FAILING` only once it's genuinely saturated |

`gated` is essential for propagation purposes (failure still reaches the parent's rollup) but
dispatched like a bounded, deferred call rather than a blocking one — a resolves-fast-when-healthy,
holds-when-slow, sheds-only-non-critical-when-saturated pattern, applied to Security Scan → SAST
Engine and Build → Container Registry. Because a best-effort child's cost is bounded, further
slowdown past 300ms has *zero* additional effect on its parent's queue — provable from the
parent's queue depth staying flat while the child's queue explodes. A gated child's parent stays
just as flat, for a different reason: the slow call was never the parent's to wait on in the first
place.

Health rollup only ever needs the derived essential/not distinction, computed leaves-first once
per tick so the shared leaf is evaluated exactly once — it stays completely unaware that gated
dependencies exist:

```
local == FAILING                                          -> FAILING
any ESSENTIAL child rollup == FAILING                     -> FAILING
local == DEGRADED
  OR any ESSENTIAL child rollup == DEGRADED
  OR any NON-ESSENTIAL child rollup is DEGRADED|FAILING   -> DEGRADED   (capped here)
otherwise                                                 -> OK
```

What differs for a gated node is *how its own local status is derived*. A generic node's status
comes from wait-time and error-rate thresholds tuned for calls that should resolve in milliseconds.
A gated node is deliberately designed to tolerate a long backlog, so those thresholds don't apply
to it — instead its local status is driven by backlog occupancy (queue depth over capacity),
pinned to the exact same threshold that triggers real shedding, so "the banner says FAILING" and
"the gate has started shedding" are never two numbers that can drift apart.

## Topology

```
Tier 1                    [ Pipeline Orchestrator ]
                ┌──────────────┬───────────┴───────┬──────────────┐
Tier 2   [Build & Compile] [Test Suite]     [Security Scan]  [Telemetry]
            blocking        blocking          blocking       best_effort
              │    └───────────┐  │                 │              │
Tier 3  [Container Reg]   [Artifact Store]    [SAST Engine]  [Kafka Bus]
           GATED           BLOCKING ×2          GATED          blocking
                           (shared leaf)
```

Three structural details do the heavy lifting:

- **`Artifact Store` has two blocking parents.** Build and Test both queue into the same
  channel, so it absorbs their combined load. Saturating it takes down two blocking branches
  at once, with no gate to absorb the slowdown first — which is why that scenario reaches
  `FAILING` fastest of any of them.
- **`Security Scan` and `Build` are both blocking, all the way to the Orchestrator.** A pipeline
  that can't get a SAST result or land its image can't ship, so neither hop is optional. What
  keeps the Orchestrator's own queue flat despite that is one tier down: both branches terminate
  in a *gated* call (Security Scan → SAST Engine, Build → Container Registry), so the slowness
  never travels back up past the gate.
- **The gate is what makes "essential but non-blocking" safe.** Marking Security Scan blocking
  without also gating SAST underneath it would just move the bug up one hop — the Orchestrator's
  own queue would back up instead. The gate is the reason blocking is safe here at all.

## How to read the demo

| Scenario | Inject | What happens | Global |
|---|---|---|---|
| **Baseline** | — | queues ~0, p95 well under the 2s deadline | 🟢 `OK` |
| **A — SAST slow** | 10x `sast-engine` | SAST's hold queue fills; **orchestrator's own queue stays flat** the entire time (containment proof). Left running past ~30s the backlog saturates and the banner reaches `FAILING` — but `runSuccessRateRC` stays ~100% while `runSuccessRateNormal` drops, because only Normal-priority runs get shed | 🟡 `DEGRADED` → 🔴 `FAILING` |
| **B — Registry slow** | 10x `container-registry` | The same two-stage story as A, on the Build → Container Registry branch instead | 🟡 `DEGRADED` → 🔴 `FAILING` |
| **C — Artifact outage** | 10x `artifact-store` | Build *and* Test back up together — no gate on this path — cascade reaches the root in ~10-15s | 🔴 `FAILING` |
| **D — Kafka lag** | 5x `kafka-bus` | queue depth climbs monotonically — visible consumer lag | 🟡 `DEGRADED` |
| **E — Reclassify** | during A, cycle `security-scan→sast-engine` through all three modes | `gated` (default) contains it at `DEGRADED`; `blocking` reproduces the old cascade — the Orchestrator's own queue backs up and the banner flips straight to 🔴 with zero change to load; `best_effort` recovers immediately | see mode |

Scenario E is the point of the whole project: identical physics, three classifications, three
different business outcomes on the exact same slowdown.

## Architecture

```
cmd/server/main.go          wiring, embedded static assets, graceful shutdown
internal/sim/               the simulation (pure Go, no HTTP)
  contract.go               frozen wire types + Controller interface
  topology.go               topology + tuning table
  priority.go               release-candidate vs. Normal priority, context helpers
  service.go                worker pool, queueing, blocking/best-effort/gated call semantics
  metrics.go                5s rolling window, percentiles, rates, RC/Normal run-success split
  rollup.go                 local status thresholds + rollup (pure, table-tested)
  engine.go                 tick loop, event log, snapshot cache
  scenarios.go              named presets
internal/api/               HTTP handlers + SSE
web/                        React + TypeScript dashboard
```

The simulation has no knowledge of HTTP, and the API layer depends only on the `sim.Controller`
interface — so the physics is testable headlessly and the two halves were built in parallel.

State reaches the browser over **SSE** at 5Hz. One-way push is all a dashboard needs;
WebSockets would have added a dependency and a reconnect protocol for no benefit.

## Design decisions & tradeoffs

**Real goroutines instead of a queueing-theory approximation.** A fluid model (`ρ = λ/μ`) would
have been faster to write and perfectly smooth. Real worker pools mean the cascade is genuinely
emergent — and it also means real bugs are possible, so `go vet` and manual review carry the
concurrency-safety burden that `-race` normally would (this dev box has `CGO_ENABLED=0`, and
`-race` requires cgo — worth noting rather than silently pretending the flag ran). Worth it: the
entire claim of the project is that this is real, and a fluid model would have been an animation
of the thesis rather than a demonstration of it.

**The gate reuses the existing worker pool as its own backlog, instead of building a second
queue.** `service.gatedCall` resolves through a short synchronous grace window — identical to the
existing best-effort call shape, so a healthy gate behaves exactly like a normal call — and only
diverges when that window elapses: the job stays in the *same* buffered channel, and a detached
goroutine with its own long-lived deadline takes over waiting for it. The alternative (a bespoke
`heldCount`/`holdCapacity` counter alongside the queue) would have been more explicit but would
have created a second source of truth for occupancy that could drift from the channel's real
depth — the whole reason gate status is keyed to `len(s.queue)` directly.

**Status derived from sustained signals, not from the injected knob.** Reading the multiplier
directly would make status flip instantly and look fake. Deriving it from a 5-second window of
queue wait and error rates costs a few seconds of latency — which is the honest behaviour.

**`Reset` doesn't force-drain queues.** Recovery takes a few seconds as the backlog clears.
That's a feature: real systems don't recover the instant the cause is removed.

**Load shedding over blocking.** A full queue rejects immediately rather than applying
backpressure to the caller. This is the more honest failure mode for a request-driven system
and gives a clean `rejected/s` signal.

**Cancellation is propagated and measured.** When a parent abandons a best-effort call at
300ms, the child's queued job is discarded when dequeued rather than burning capacity — and
that shows up as `abandoned/s`.

**Fixed topology, no graph editor.** A configurable DAG would have been more general and much
less pointed. Nine nodes chosen so that every edge tells a specific story is more persuasive
than an empty canvas.

## Running it

```bash
# backend (serves embedded UI on :8080)
go run ./cmd/server

# frontend dev server with hot reload, proxies /api to :8080
cd web && npm install && npm run dev

# tests (add -race if your platform has cgo available; this dev box doesn't)
go test ./...
cd web && npm test
```

A few of the Go tests deliberately run the gated scenarios to real saturation and take up to
~90s; they're skipped under `go test -short`.

Deployment to Cloud Run: see [DEPLOY.md](DEPLOY.md).

## What I'd do with more time

- **Priority-ordered draining of the gate's backlog.** Right now "release-candidate is protected"
  means RC calls are never *shed*; it does not mean they jump the line ahead of already-queued
  Normal calls once the gate recovers capacity. A priority queue instead of a plain FIFO channel
  would make that ordering real, closer to how a production release-gate would actually prioritize
  a backlog once it starts draining.
- **Circuit breakers and retry/backoff** layered on top of the timeouts — the current
  containment is a plain timeout; a breaker would stop wasting capacity on a known-dead
  dependency, and retry storms are the classic way a partial outage becomes a total one.
- **Adaptive concurrency limits** (AIMD, à la Netflix's `concurrency-limits`) instead of fixed
  worker pools, so the system finds its own safe operating point under stress.
- **A "what-if" mode** — given the current topology, compute which single node failures can
  reach `FAILING`. That turns the toy into something resembling a real dependency-risk audit.
- **Historical timeline** with the incident replayable, so you can scrub through a cascade
  rather than having to catch it live.
- Per-node worker-count and queue-capacity tuning from the UI, to make the capacity/latency
  tradeoff directly explorable.
