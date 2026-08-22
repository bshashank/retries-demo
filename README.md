# Pipeline Health — Cascading Degradation Under Load

A live dashboard of a 9-node CI/CD pipeline where **every service is a real Go worker pool**.
Slow down one leaf node and watch the degradation propagate up the dependency graph in real
time — or fail to propagate, depending on how the dependency was classified.

**Live demo:** _(Cloud Run URL — see DEPLOY.md)_

---

## The idea in one paragraph

Most reliability demos show a service failing. This one shows why **the same failure produces
two completely different global outcomes** depending on whether the broken dependency was
marked *essential* or *non-essential* — and lets you flip that classification live, mid-incident,
while the underlying load stays identical. Watching a red node leave the global banner green is
the moment the concept lands.

## Why it isn't a scripted animation

Nothing here is a state machine with hardcoded transitions. Each of the 9 services is a real
worker pool: a **buffered Go channel is the queue**, N goroutines are the workers, and a load
generator drives ~20 pipeline runs/sec through the DAG continuously.

When you slow a leaf down 10x:

1. That node's workers take longer per job, so its capacity drops below its arrival rate.
2. Its buffered channel — a real queue — starts filling. Queue wait time climbs.
3. Its **essential** parent blocks on it with the full remaining request deadline, so the
   parent's workers stay occupied longer, so the *parent's* capacity drops too.
4. The parent's queue starts filling. The cascade climbs one tier at a time.
5. Once queues hit capacity, enqueues are shed rather than blocked — a visible `rejected/s`.

Health is derived from *sustained* signals (mean queue wait, error/reject/abandon rates over a
5-second rolling window), never from the injected multiplier directly. That is deliberate: it
means status changes lag the injection by seconds, the way real incidents do, instead of
flipping the instant you touch a control.

## The essential / non-essential asymmetry

This is the whole mechanism, and it lives in one place — how a parent calls a child:

| | Essential dependency | Non-essential dependency |
|---|---|---|
| Context | caller's full remaining deadline | `context.WithTimeout(ctx, 300ms)` |
| Parent behaviour | **blocks** until it answers | gives up at 300ms, proceeds anyway |
| Child latency cost to parent | **unbounded** — fully additive | **capped at 300ms** |
| On child failure | parent fails, propagates up | parent sets `degraded`, keeps serving |
| Worst global outcome | `FAILING` | `DEGRADED` — capped, structurally |

Because a non-essential child's cost is bounded, further slowdown past 300ms has *zero*
additional effect on the parent's queue. The containment isn't cosmetic — it's provable from
the parent's queue depth staying flat while its child's queue explodes.

Health rollup, computed leaves-first once per tick so the shared leaf is evaluated exactly once:

```
local == FAILING                                          -> FAILING
any ESSENTIAL child rollup == FAILING                     -> FAILING
local == DEGRADED
  OR any ESSENTIAL child rollup == DEGRADED
  OR any NON-ESSENTIAL child rollup is DEGRADED|FAILING   -> DEGRADED   (capped here)
otherwise                                                 -> OK
```

## Topology

```
Tier 1                    [ Pipeline Orchestrator ]
                ┌──────────────┬───────────┴───────┬──────────────┐
Tier 2   [Build & Compile] [Test Suite]     [Security Scan]  [Telemetry]
            essential       essential        NON-essential   NON-essential
              │    └───────────┐  │                 │              │
Tier 3  [Container Reg]   [Artifact Store]    [SAST Engine]  [Kafka Bus]
         non-essential     ESSENTIAL ×2         essential      essential
                           (shared leaf)
```

Two structural details do the heavy lifting:

- **`Artifact Store` has two essential parents.** Build and Test both queue into the same
  channel, so it absorbs their combined load. Saturating it takes down two essential branches
  at once — which is why that scenario reaches `FAILING` and the others don't.
- **`SAST Engine` is essential to `Security Scan`, which is itself non-essential to the
  Orchestrator.** So SAST dying turns that entire branch red while the global banner stays
  amber. Criticality is a property of the *path*, not the node — that's the non-obvious part.

## How to read the demo

| Scenario | Inject | What happens | Global |
|---|---|---|---|
| **Baseline** | — | queues ~0, p95 well under the 2s deadline | 🟢 `OK` |
| **A — SAST slow** | 10x `sast-engine` | SAST + Security Scan go red; **orchestrator queue stays flat** (containment proof) | 🟡 `DEGRADED` |
| **B — Artifact outage** | 10x `artifact-store` | Build *and* Test back up together, cascade reaches the root in ~10-15s | 🔴 `FAILING` |
| **C — Kafka lag** | 5x `kafka-bus` | queue depth climbs monotonically — visible consumer lag | 🟡 `DEGRADED` |
| **D — Reclassify** | during A, mark `orchestrator→security-scan` essential | banner flips 🟡→🔴 **with zero change to load** | 🔴 `FAILING` |

Scenario D is the point of the whole project: identical physics, different classification,
different business outcome.

## Architecture

```
cmd/server/main.go          wiring, embedded static assets, graceful shutdown
internal/sim/               the simulation (pure Go, no HTTP)
  contract.go               frozen wire types + Controller interface
  graph.go                  topology + tuning table
  service.go                worker pool, queueing, essential/non-essential call semantics
  metrics.go                5s rolling window, percentiles, rates
  health.go                 local status thresholds + rollup (pure, table-tested)
  engine.go                 tick loop, event log, snapshot cache
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
emergent — and it also means real bugs are possible, so the tests run under `-race`. Worth it:
the entire claim of the project is that this is real, and a fluid model would have been an
animation of the thesis rather than a demonstration of it.

**Status derived from sustained signals, not from the injected knob.** Reading the multiplier
directly would make status flip instantly and look fake. Deriving it from a 5-second window of
queue wait and error rates costs a few seconds of latency — which is the honest behaviour.

**`Reset` doesn't force-drain queues.** Recovery takes a few seconds as the backlog clears.
That's a feature: real systems don't recover the instant the cause is removed.

**Load shedding over blocking.** A full queue rejects immediately rather than applying
backpressure to the caller. This is the more honest failure mode for a request-driven system
and gives a clean `rejected/s` signal.

**Cancellation is propagated and measured.** When a parent abandons a non-essential call at
300ms, the child's queued job is discarded when dequeued rather than burning capacity — and
that shows up as `abandoned/s`, which is what drives Security Scan's status in scenario A.

**Fixed topology, no graph editor.** A configurable DAG would have been more general and much
less pointed. Nine nodes chosen so that every edge tells a specific story is more persuasive
than an empty canvas.

## Running it

```bash
# backend (serves embedded UI on :8080)
go run ./cmd/server

# frontend dev server with hot reload, proxies /api to :8080
cd web && npm install && npm run dev

# tests
go test ./... -race
cd web && npm test
```

Deployment to Cloud Run: see [DEPLOY.md](DEPLOY.md).

## What I'd do with more time

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
