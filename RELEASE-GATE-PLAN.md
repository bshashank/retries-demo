# Correcting the release-gate degradation model (SAST + Container Registry)

## Context

The deployed prototype (`pipeline-health`, Go backend + React frontend, live at
Cloud Run) models health as OK/DEGRADED/FAILING via an essential/non-essential
edge classification. Two edges are misclassified the same way, both
identified by the user from real CI/CD experience:

- `Orchestrator → Security Scan`: today **non-essential** (300ms timeout,
  proceed regardless) — asserts a security scan is skippable. A pipeline that
  can't get a SAST result can't ship.
- `Build & Compile → Container Registry`: today **non-essential** ("registry
  push is best-effort") — asserts the registry push doesn't matter. CD can't
  trigger a deploy without the image actually landing in the registry.

Both are essential *for a real release* — but real systems don't handle that
by blocking a worker for hours (naive "essential") or by silently skipping
the gate (today's bug). They hold the run in a bounded backlog awaiting
recovery, and — only once that backlog saturates — shed non-release-candidate
(non-RC) runs to protect RC capacity. Confirmed with the user: Kafka Event
Bus / Telemetry Reporter are *not* part of this correction — those genuinely
can fail with the pipeline still marked a success, exactly as modeled today.

Goal: reclassify both edges and implement one reusable bounded-hold +
priority-aware-shedding mechanism, applied to both, reusing the existing
worker-pool/queue/timeout/rollup architecture rather than building a new
subsystem. Confirmed with the user: full implementation (not docs-only), RC
ratio ~10% of load-generator traffic.

## The mechanism

Key insight from tracing the existing code: an "essential" edge today
inherits the **caller's full context**, itself capped by the 2s `runDeadline`
set in `engine.go`'s `generateLoad()`. So marking an edge essential today
only ever buys ~2s of patience, nowhere close to "hold for hours." The fix
is a genuinely different call shape, built as a **hybrid of the existing
non-essential grace-timeout pattern**, extended so a timed-out call is
*promoted to a tracked background attempt* instead of abandoned.

Gate state belongs to **the resource being gated, not the caller** — added to
`service` (SAST Engine, Container Registry both get it; every other node
leaves it unset): `holdCapacity int`, `heldCount atomic.Int64`, `gateGrace`,
`gateHoldBudget time.Duration`. This is what lets the *existing* rollup
propagation handle everything downstream for free: a saturated gate is just
that node's local status going FAILING, and essential-child-FAILING-
propagates-to-parent already exists in `rollup.go` — no second propagation
path to hand-build.

`service.gatedCall(ctx, priority) Result` — the child's new entry point,
called instead of `call()` when the edge's mode is `gated`:

1. Try a real call bounded by a short grace window (reuse `nonEssentialTimeout`,
   300ms). Resolves in time → return the real result. This is what keeps
   baseline behavior (SAST/Registry healthy) completely unchanged.
2. Grace window elapses, `heldCount < holdCapacity` (or caller is
   ReleaseCandidate, which always bypasses the cap) → **promoted**: increment
   `heldCount`, launch a detached goroutine with its own `gateHoldBudget`
   (independent of the caller's ctx — the caller's 2s run deadline is about
   to expire regardless, and detaching is what makes "wait a long time"
   possible without blocking a worker). Decrement `heldCount` when it
   eventually resolves. Return `Result{Degraded: true}` to the caller
   immediately — the original run completes on schedule, marked pending.
3. Grace window elapses, `heldCount >= holdCapacity`, caller is Normal →
   **shed**: reuse `ErrQueueFull` + `metrics.RecordRejected()` (already wired
   into the existing reject-rate UI). This is a real, hard failure for that
   specific run.

**This is the only place priority matters, and it's deliberately narrow**:
RC is never shed, full stop. I'm not making a promoted call's *eventual*
background outcome retroactively change the original run's result (that
would require blocking the original request for however long the gate
takes, undoing the whole point). "Replayed" just means the backlog keeps
draining in the background; the visible, *measured* proof is
`RunSuccessRateNormal` dipping under sustained pressure while
`RunSuccessRateRC` stays flat — because shedding is the only path that
produces a hard failure, and only Normal traffic can be shed.

One real consequence worth being deliberate about in the UI: the dashboard's
aggregate status can reach FAILING (SAST or Registry saturating) at the same
time `RunSuccessRateRC` stays high — "the banner says FAILING, but release
traffic is still getting through" is an accurate picture of a real incident,
not a contradiction, but `insights.ts`/`CausalDiagnosticCard` needs to say so
explicitly rather than leave it looking like a bug.

### Why essential/non-essential doesn't disappear — it becomes one axis of a 3-way mode

Raised directly by the user: if these edges become essential, does the
essential/non-essential distinction still mean anything? Tracing it through:
the original model bundles two separate questions into one bit — *does a
failure here propagate* (correctness) and *does this call block the caller*
(dispatch/latency) — and silently assumes essential⇒blocking,
non-essential⇒timeout-and-skip. SAST/Registry are counterexamples: essential
for correctness, but explicitly must not block. A second, independently
toggleable `gated` bool bolted onto `essential` would just recreate a
redundant-flag problem (cells that don't correspond to anything real, like
gated+non-essential).

The fix: replace the boolean with a single 3-valued `DependencyMode`:

| Mode | Essential? (derived) | Dispatch | Edges |
|---|---|---|---|
| `blocking` | yes | synchronous, unbounded latency pass-through | Orchestrator → Build/Test, Build/Test → Artifact Store, Orchestrator → Security Scan |
| `best_effort` | no | synchronous, 300ms timeout, proceed-and-degrade | Orchestrator → Telemetry, Telemetry → Kafka |
| `gated` | yes | grace-window probe, then deferred/bounded-backlog, priority-aware shedding | Security Scan → SAST, Build → Container Registry |

"Essential" is now a *derived* property (`mode != best_effort`), not a
separately-stored bit — restoring the "one tag fully determines behavior"
property the demo's headline claim depends on. `rollup.go` is unaffected: it
only ever needs the derived essential/not distinction via a small helper
(`modeEssential(mode) bool`), never the mode itself — the "only place one
node's status influences another's" invariant stays intact. `service.go`
dispatches on the 3 modes directly in `process()`.

This also makes the live-reclassification toggle strictly more useful: on
either gated edge, cycling through all three modes during an outage shows
three different outcomes on identical load — `best_effort` (skips it, always
wrong), `blocking` (correct but cascades), `gated` (correct and contained) —
demonstrating gated is the right choice instead of just asserting it.

A gate-holding node's **local status** stops being driven purely by its
generic queue-wait/error-rate (which looks nominal now, since its own
processing is unaffected by its callers' patience) and instead also folds in
gate occupancy: a new pure function `gateStatus(held, capacity int) Status`
(empty→OK, building→DEGRADED, at capacity/shedding→FAILING), combined via a
new `severity()` helper against the existing `localStatus(v)` result, taking
the worse of the two. This keeps the well-tested generic `localStatus`
untouched for every other node.

**Explicitly scoped out** (note as a stretch in the docs, don't build): true
priority-*ordered draining* (RC jobs jumping ahead of already-queued Normal
jobs once a gate recovers). Core behavior is "RC is never shed"; ordering
within the backlog is not implemented.

## Files to change

**Backend (`internal/sim/`)**
- `priority.go` (new) — `Priority` type (`PriorityNormal`, `PriorityReleaseCandidate`), `rcRatio = 0.10` constant, `withPriority`/`priorityFromContext` context helpers, and a `pickPriority()` rng helper.
- `contract.go` — new `DependencyMode` string type (`ModeBlocking`, `ModeBestEffort`, `ModeGated`) with a `modeEssential(mode) bool` helper. `EdgeSnapshot.Essential bool` → `EdgeSnapshot.Mode DependencyMode` (replacement, not additive — one tag, not a tag plus an independently-stored derived bool). Add `HeldCount`, `HoldCapacity int` to `NodeSnapshot` (populated for SAST Engine and Container Registry only). Add `RunSuccessRateRC`, `RunSuccessRateNormal float64` to `Snapshot`. `Controller.SetEdgeEssential(from, to string, essential bool)` → `SetEdgeMode(from, to string, mode DependencyMode) error`.
- `service.go` — `dependency.essential atomic.Bool` → `dependency.mode atomic.Value` (holding `DependencyMode`, still runtime-toggleable — powers the 3-way live-reclassification experiment). Add `holdCapacity int`, `heldCount atomic.Int64`, `gateGrace`, `gateHoldBudget time.Duration` to `service` (only SAST Engine and Container Registry get them populated). Add `service.gatedCall(ctx, priority) Result` implementing the mechanism above. `process()`'s dependency fan-out switches on `dep.mode.Load()`: `ModeBlocking` → today's blocking call; `ModeBestEffort` → today's 300ms-timeout call; `ModeGated` → `dep.child.gatedCall(ctx, priority)`.
- `topology.go` — `edgeSpec.Essential bool` → `edgeSpec.Mode DependencyMode`; `nodeSpec` gains optional `HoldCapacity int`, `GateGrace`, `GateHoldBudget time.Duration` (zero ⇒ no gate). `edgeSpecs()`: `{SecurityScan, SAST}` and `{Build, ContainerRegistry}` → `ModeGated`; `{Orchestrator, SecurityScan}` → `ModeBlocking` (was `ModeBestEffort` — the actual bug); everything else keeps its current blocking/best_effort assignment under the new names. `nodeSpecs()` sets gate config on the SAST Engine and Container Registry entries. `validateTopology` gets one new invariant: an edge may use `ModeGated` only if its target node has gate config. Tune both `holdCapacity`s live (starting guess: sized so a full outage at the essential-now arrival rate fills each in ~15-20s, consistent with the pacing of the other scenarios).
- `rollup.go` — `rollupDep.Essential bool` stays exactly as-is conceptually, now populated via `modeEssential(dep.mode.Load())` at the one call site in `engine.go`'s `tick()`. `rollupOne`/`localStatus` are unchanged — the point of keeping essential-ness as a derived helper rather than threading `DependencyMode` through the rollup logic. Add `gateStatus(held, capacity int) Status` (pure, table-testable like `localStatus`) and `severity(Status) int`.
- `engine.go` — `generateLoad()` tags each run's context with a priority via `withPriority` before calling `e.root.call(runCtx)`; `runMetrics`/`runView` (in `metrics.go`) gets a priority field on `runSample` and splits `SuccessRate` into RC/Normal in `Read()`. `tick()` builds `rollupDep.Essential` via `modeEssential(...)`, combines `localStatus(v)` with `gateStatus(...)` for any node carrying gate config, and populates the new `NodeSnapshot`/`EdgeSnapshot`/`Snapshot` fields. `SetEdgeEssential` → `SetEdgeMode`.
- `scenarios.go` — rewrite `ScenarioSASTSlow`'s `Description`/`Expected` for the two-stage story (DEGRADED while the gate has headroom, escalating to FAILING with visible non-RC shedding if left running) instead of the retired "must never reach FAILING" claim. Add an equivalent registry-outage scenario (or extend an existing one) so both gated edges are reachable via a preset, not only manual injection. Retune multipliers alongside both `holdCapacity`s so each stage is reachable within a normal demo interaction window.

**Frontend (`web/src/`)**
- `types.ts` — mirror the new `contract.go` shapes exactly (comment already states this file must match 1:1): `DependencyMode` union type, `EdgeSnapshot.mode` replacing `essential`, new `NodeSnapshot`/`Snapshot` fields, `EdgeRequest.mode` replacing `essential`.
- `lib/insights.ts` — every `edge.essential` check becomes a derived `isEssential(edge.mode)` check (mirrors the Go-side `modeEssential` helper). `computeSystemDiagnostic` gets a third `activeChains` type (e.g. `'gated_hold'`) for `mode === 'gated'` edges, with its own description distinct from `essential_escalation` (blocking) and `contained_isolation` (best_effort): bounded backlog, RC-exempt shedding — and an explicit note for the "FAILING banner + high RC success rate" case so it reads as intentional, not broken.
- `components/ControlPanel.tsx` — the per-edge essential toggle becomes a 3-way mode control; both gated edges expose all three modes (the "watch the same slowdown produce three different outcomes" experiment).
- `components/DagView.tsx` — edge line styling needs a third visual treatment for `gated` (currently solid=essential/dashed=non-essential; gated needs its own, e.g. dash-dot).
- `components/NodeCard.tsx` — when `node.holdCapacity > 0`, render an additional occupancy bar for the gate (reuse the existing `queueFillPercent` helper from `lib/format.ts`, already used for the node's own queue bar).
- Wherever `runSuccessRate`/`runP95Ms` currently surface (`GlobalHealthBanner.tsx` or `App.tsx`) — add the RC/Normal split so "RC never gets shed" is visible as a number, not just inferred.
- `lib/status.test.ts` (or a new `insights.test.ts`) — cover `isEssential` and the new `gated_hold` branch the same way existing branches are covered.

## Verification

1. `go vet ./...`, `go test ./...` (no `-race` available on this Windows box — no cgo — same limitation as before; note it rather than fight it).
2. New/updated Go table tests: `gateStatus` thresholds in `rollup_test.go`; RC-bypass-at-capacity and Normal-shed-at-capacity in `service_test.go` (use small injectable `gateGrace`/`gateHoldBudget` per test, same spirit as `metrics.now` being injectable, so tests stay fast); the new topology validation rule in `topology_test.go`.
3. `cd web && npm test`.
4. Live smoke test both gates the same way the artifact-outage scenario was verified earlier this session: `go run ./cmd/server`, drive the SAST scenario and poll `/api/snapshot`, confirming baseline OK (heldCount ~0) → DEGRADED (gate holding) → FAILING (saturated) with `rejectRate` rising on SAST Engine and `runSuccessRateNormal` dropping while `runSuccessRateRC` stays high, then recovery after `/api/reset`. Repeat for the registry gate. Also drive `/api/edge {"from":"security-scan","to":"sast-engine","mode":"blocking"}` mid-outage and confirm it reproduces the old cascade, then `"mode":"best_effort"` and confirm it caps at DEGRADED — the three-mode comparison is the core new demo moment.
5. `npm run build` in `web/`, then a full local run through the UI (`run` skill or manual) to confirm both gate bars, the 3-way mode control, and the RC/Normal split render correctly, before redeploying to Cloud Run and re-verifying on the live URL the same way the artifact-outage scenario was just verified live.
6. Update `README.md`/`PLAN.md`: replace the retired "SAST slowness must never reach FAILING" claim with the corrected two-stage story for both gates, explain the "FAILING banner + high RC success" nuance explicitly, and note the RC-ordering stretch goal in "what I'd do with more time."
