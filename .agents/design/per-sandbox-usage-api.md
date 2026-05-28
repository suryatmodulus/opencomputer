# Per-sandbox usage API

Replace `GET /api/sandboxes/{id}/usage` with a points-by-default shape
that exposes actual resource utilization (not just allocation) at 1m
resolution. One endpoint serves both programmatic queries and chart
rendering. The data already exists in `sandbox_usage_samples` — this is
a new query/response layer over data the worker is already collecting.

## Why: today's endpoint only answers the billing question

`GET /api/sandboxes/{id}/usage` today returns:

```json
{ "memoryGbSeconds": ..., "diskOverageGbSeconds": ..., ... }
```

`memoryGbSeconds` is **allocated** memory (`sandbox_scale_events.memory_mb`)
integrated over the window. That's the right answer to "what did this
sandbox cost?" but the wrong answer to every other question a developer
asks about their own sandbox:

- *"Did this sandbox actually need the memory I provisioned, or am I
  paying for headroom?"* — needs `memory_bytes` (cgroup `memory.current`)
- *"When was the spike?"* — needs a time series
- *"Show me memory over the last day, charted"* — needs both shape
  and data the current endpoint doesn't expose

`memory_bytes` is already collected every 60s in `sandbox_usage_samples`
(migration 015, worker `usage_collector.go`). It has never been exposed
via the API. The drilldown endpoint reads only from
`sandbox_scale_events` and ignores the samples table entirely.

**v1 is memory-only.** CPU mirrors the same shape and joins the same
samples table but requires a collector fix first (see Out of scope).
The API shape below is designed so the CPU fields slot in symmetrically
when the collector lands — no breaking change at that point.

This redesign closes that gap and replaces the response shape with one
that serves programmatic consumers and dashboard charts from a single
call.

## Shape: points + totals, no knobs

```
GET /api/sandboxes/{id}/usage?from=<RFC3339>&to=<RFC3339>
```

```json
{
  "sandboxId": "sb-abc",
  "alias": "my-agent",
  "from":   "2026-05-22T00:00:00Z",
  "to":     "2026-05-23T00:00:00Z",
  "totals": {
    "memoryAllocatedGbSeconds":  8000,
    "memoryUsedGbSeconds":       5400,
    "uptimeSeconds":            86400,
    "memoryAllocatedPeakMb":     2048,
    "memoryUsedPeakMb":          1640
  },
  "points": [
    {
      "ts":                        "2026-05-22T00:00:00Z",
      "memoryAllocatedGbSeconds":      1.024,
      "memoryUsedGbSeconds":           0.612,
      "uptimeSeconds":                60,
      "allocatedMemoryMb":           1024,
      "usedMemoryMbAvg":              612,
      "usedMemoryMbPeak":             720
    },
    { "ts": "2026-05-22T00:01:00Z", ... },
    ...
  ]
}
```

One response. Charts read `points[]`. Programmatic consumers do
arithmetic on either `totals` or `points[]`. No `resolution=`, no
`timeseries=` opt-in, no `groupBy`. The window IS the granularity
choice — for finer detail, ask for a shorter range.

### Why each point carries integrals *and* snapshots

Each point includes both **time-integrated quantities** (`*GbSeconds`,
`uptimeSeconds`) and **snapshot scalars** (`allocatedMemoryMb`,
`usedMemoryMb{Avg,Peak}`).

- Integrals are what compose. Sum point-level `memoryUsedGbSeconds`
  across any subrange and you get the integral over that subrange.
  This is the property `totals` exploits.
- Snapshots are what charts want. A line at "1.2 GB" is easier for a
  human to read than a line at "72 GB-seconds-per-minute."

Both come from the same underlying samples; emitting both costs almost
nothing and means clients never have to do
`gbSeconds * 1024 / 60` arithmetic to render a chart.

### Time-weighted average, not last-value

`allocatedMemoryMb` inside each point is a time-weighted average over
the bucket, not the value at the bucket's end. Reason: when a resize
event falls inside a bucket, the average correctly attributes part of
the bucket to the old tier and part to the new one. Last-value would
lie about the bucket's interior, and charts would draw a clean step
that doesn't match the GB-seconds math.

The visual result: resize shows up as a single bucket with a
fractional value, then the new tier from the next bucket on. Acceptable
because resize is rare relative to the 1m bucket.

## Window

Optional: `from`, `to`. Both RFC3339. When omitted, server defaults
to `from = now - 1h`, `to = now`. Max: **30 days**. `to` must be
strictly greater than `from`.

Boundary buckets are clamped to the original window — when `from`
falls mid-minute, the first bucket's allocation and uptime reflect
only the portion that actually overlaps `[from, to)`, not the full
minute it nominally covers. Without this clamp, a default
`now-1h..now` call would over-report by up to two full minutes.

At 1m granularity, 30 days = 43,200 points ≈ 5 MB uncompressed JSON,
~300–500 KB gzipped. Trivial for browsers and SDK consumers. The
practical mode is shorter windows (1h–7d for charts; longer for
analytics scripts that don't actually want point-level data and should
be using `/api/usage` instead).

The 30d cap exists because the data is retained at sample-level for
the billing pipeline already, and exposing the full retention through
the API gives analytics scripts a natural way to do month-over-month
without inventing batching. Beyond 30d, point-by-point is the wrong
shape — use the aggregator.

## Bucket: always 1m

No client knob, no server-side tiering. The bucket equals the sample
cadence (60s, set in `usage_collector.go`). Reasons:

- Sample rate is already 60s — picking any other bucket size means
  aggregating samples on read, which is overhead with no information
  gain. Sub-minute would require sampling more often, which costs more
  to collect and store.
- A fixed bucket means clients never have to think about resolution,
  never have to handle `"got 5m bucket when I asked for 1m"`, and can
  always sum N points to get the next coarser window.
- Payload bound is already acceptable at the 30d cap. Tiering would
  add API surface area to shave a megabyte at the worst case.

If chart UIs need coarser visualization at long ranges, they downsample
client-side. SDKs may ship a helper, but the wire format stays 1m.

## Gaps: emit zero, never null

For minute buckets where the sandbox was not running (between
hibernation and resume, after stop and before destroy, etc.), the
point is emitted with **all integrals = 0**, `uptimeSeconds = 0`,
`allocatedMemoryMb = 0`, `usedMemoryMbAvg = 0`. Reasons:

- Charts read continuous x-axes without phantom interpolation across
  nulls.
- Downtime is information — visible as a flat-zero stretch on the
  chart. A null gap is ambiguous (did it not run, or did the API not
  know?).
- Sums stay correct without client-side null handling.

For minutes that fall entirely outside the sandbox's lifetime (before
`firstStartedAt`, after `destroyedAt`), zero is still correct — there
was no sandbox, so no usage. The response always covers the full
`[from, to]` range regardless of when the sandbox existed within it.
The handler clamps internally: if the window extends before the
sandbox's first session, those minutes are emitted as zero-points.

## What gets dropped from the existing endpoint

The current shape exposes `diskOverageGbSeconds`, `tags`,
`tagsLastUpdatedAt`, `firstStartedAt`, `lastEndedAt`, `status`,
`alias`. The new shape drops the disk overage metric and the
tag/lifetime hydration:

- **`diskOverageGbSeconds`**: belongs to billing reconciliation, not to
  utilization. It stays on `/api/usage` where billing consumers
  already read it. Disk is provisioned, not measured continuously —
  there is no `memory_bytes` analogue to surface.
- **`tags`, `tagsLastUpdatedAt`, `status`**: these are identity, not
  usage. They belong on the sandbox detail endpoint
  (`GET /api/sandboxes/:id`), which already returns them. Bundling
  them here forces every usage call to re-hydrate metadata that
  doesn't change in a usage-relevant way.
- **`alias`**: dropped from the strict-narrowing rationale above but
  kept inline as an exception — it's a 2-line lookup from the
  sandbox_sessions config JSONB, and CLI/dashboard contexts
  ("which sandbox am I looking at?") read much better with it
  present than with a follow-up call to `GET /api/sandboxes/:id`.
- **`firstStartedAt`, `lastEndedAt`**: derivable from the points
  array (first/last point with `uptimeSeconds > 0`). The aggregate
  fields go away; the data they represented is still recoverable.

This is a deliberate narrowing: the endpoint becomes "usage of this
sandbox" full stop, not "everything about this sandbox plus its bill."

## What stays in `/api/usage` (the aggregator)

Unchanged shape, unchanged math, unchanged billing reconciliation
invariants. `groupBy=sandbox|tag:<key>`, filter/sort/cursor — all
preserved. That endpoint serves the leaderboard / who-spent-most
question; this one serves the per-sandbox time-series question. Two
endpoints, two natural shapes, no overlap.

Additive change to consider in a follow-up (not part of this design):
add `sort=-memoryUsedGbSeconds` on `/api/usage` so the upsell-signal
leaderboard ("rank by *actual* usage, not by bill") is one query
away. Requires reading the samples table in the aggregator query;
reuses the same window/filter machinery. **This follow-up is what
directly answers the "find heavy users" half of the original product
ask** — the per-sandbox endpoint here answers the "look at one
sandbox" half. Together they cover the surface.

## Backwards compatibility

The existing response shape is incompatible with the new one (flat
scalars vs points + totals envelope). Path:

1. Cut the new endpoint at `/api/sandboxes/{id}/usage` as a clean
   replacement. The Python and TypeScript SDKs bump major versions; a
   tagged release captures the old client for any caller pinned to the
   prior shape.
2. The billing-derived field `memoryGbSeconds` from the old shape maps
   to `totals.memoryAllocatedGbSeconds` in the new shape (same number,
   same math, just renamed for clarity now that an actual-usage
   counterpart exists alongside it).
3. `/api/usage` is untouched — billing reconciliation consumers (Stripe
   pipeline tests, internal billing scripts) are not affected.

## Data dependencies and changes required

**Existing, sufficient:**

- `sandbox_scale_events` — interval-table for allocation; resize
  already handled correctly.
- `sandbox_usage_samples` — 60s samples with `memory_mb` (allocated
  tier), `memory_bytes` (cgroup `memory.current`, the actual usage),
  `pids`. Indexed on `(org_id, sampled_at)` and primary-keyed on
  `(sandbox_id, sampled_at)`.

**No collector changes required for v1.** Memory is already being
collected end-to-end. CPU is deferred — see Out of scope for the
collector TODO that has to be cleared before CPU joins the response.

**Query shape:**

The handler builds points by joining `sandbox_usage_samples` (one row
per minute when the sandbox was running) to `sandbox_scale_events`
(for the allocated tier in effect at each sample timestamp) and
filling in zero-points for minutes the sample table is missing
within `[from, to]`. The simplest implementation is a `generate_series`
over the window at 1m intervals, LEFT JOINed to samples and
LEFT JOINed to the active scale event per minute, with everything
COALESCEd to zero.

`memoryUsedGbSeconds` per point = `memory_bytes / 1e9 * 60`.
`memoryAllocatedGbSeconds` per point = `memory_mb / 1024 * 60` (when
running; 0 otherwise). `uptimeSeconds` = 60 if a sample exists for
that minute, else 0.

`totals` is computed by summing the materialized points server-side
(not by a second aggregate query) so the published invariant
"sum of point integrals == totals" is enforced by construction.

## Worked example

A developer creates a 2 GB sandbox, runs a workload that uses ~600 MB
average and peaks at ~800 MB. After 6 hours they upsize to 4 GB,
peak rises to 1.4 GB. After another 18 hours they stop the sandbox.
They `GET /api/sandboxes/sb-abc/usage?from=...&to=...` for the full
24h window and get:

- `totals.memoryAllocatedGbSeconds` ≈ 2 GB × 6h × 3600 + 4 GB × 18h × 3600
  ≈ 302,400 — what the bill is computed from.
- `totals.memoryUsedGbSeconds` ≈ 0.6 GB × 6h × 3600 + 1.1 GB × 18h × 3600
  ≈ 84,240 — what they actually used.
- `totals.memoryAllocatedPeakMb` = 4096; `memoryUsedPeakMb` ≈ 1400.

The headroom story is visible without opening a chart: ~28% utilization
average, peak well under the new tier. If they open the chart from
`points[]`, the resize is a step in `allocatedMemoryMb` at the 6h
mark, with the noisy `usedMemoryMbAvg` curve underneath.

## Testing strategy

**Handler / DB fixture tests** (`internal/api`, `internal/db`):

- Generate-series query with seeded fixtures: given a known set of
  `sandbox_scale_events` and `sandbox_usage_samples` rows, the
  emitted `points[]` must match expectations field-by-field. The DB
  seed is the input contract; the response is the output.
- Time-weighted allocation across a resize: seed two scale events
  whose boundary falls inside a 1m bucket, assert the bucket's
  `allocatedMemoryMb` is the weighted average and the next bucket
  shows the new tier.
- Gap handling: a 5-minute window with samples only at minutes 1 and
  4 produces 5 points; minutes 2, 3, 5 emit zero-points with no
  nulls anywhere.
- Window clamping: `from` predates the sandbox's first session →
  leading minutes are zero-points, not missing rows.
- Validation: `to <= from` → 400; `to - from > 30d` → 400; unknown
  sandbox ID or one belonging to another org → 404 (existing
  `ownsSandbox` check covers this).
- Totals invariant: `SUM(points[].memoryUsedGbSeconds)` equals
  `totals.memoryUsedGbSeconds` exactly (computed server-side from the
  same materialized points, so this is testing the contract, not the
  math).

**Reconciliation against the existing billing query — and one
intentional divergence:**

`totals.memoryAllocatedGbSeconds` equals the existing `/api/usage`'s
`memoryGbSeconds` for the same `[from, to]` and sandbox **when all
in-window scale events terminate at or before `to`**. Both derive from
`sandbox_scale_events` with the same integration math (and the same
`COALESCE(ended_at, LEAST(now(), to))` clamp for open events), so the
common case is bug-for-bug identical and the Stripe pipeline keeps
its reconciliation invariant.

The two endpoints **disagree by design when an event's `ended_at > to`**.
The existing aggregator does not clamp `ended_at` to `to` (an inherited
quirk in `GetOrgUsage` documented at `internal/db/usage_query.go`);
overflow time accrues into the "in-window" total. The new endpoint
clamps via the bucket-edge `LEAST(..., $4)` chain, so it reports the
mathematically correct in-window allocation while the aggregator
overshoots.

This divergence is pinned by `TestSandboxUsagePoints_OvershootDivergence_pgfixture`
so it can't drift silently. The two endpoints will converge again when
the aggregator's clamp is fixed (out of scope here).

**Units note:** every `GbSeconds` field is **GiB-seconds** (binary,
1 GiB = 2³⁰ bytes). The wire-format keeps the historical `Gb` name to
stay compatible with the existing aggregator field; do not interpret
it as decimal GB.

**End-to-end on the GCP dev box:**

The fastest validation loop is the existing dev host. Sample cadence
is 60s, flush every 5 samples — so a meaningful response is available
within ~3–5 minutes of starting a sandbox.

1. Create a sandbox, run a workload that allocates ~600 MB (a Python
   script holding a bytes buffer is enough).
2. Wait for at least one flush (~5 min). Verify rows in
   `sandbox_usage_samples` directly with `psql`.
3. `GET /api/sandboxes/{id}/usage?from=<recent>&to=<now>` and inspect
   the response: minute-resolution points, `usedMemoryMbAvg` tracks
   the workload's RSS, `allocatedMemoryMb` matches the sandbox tier,
   `uptimeSeconds: 60` per active minute.
4. Resize the sandbox mid-experiment; verify the resize-bucket has a
   fractional/weighted `allocatedMemoryMb` and subsequent buckets
   show the new tier.
5. Stop the sandbox; verify subsequent minutes inside the queried
   window emit zero-points.

This is the only path that exercises the full chain (worker
collection → batch insert → query → response) and catches integration
bugs that fixture tests miss (timezone handling, batch flush timing,
sample-vs-scale-event ordering at boundaries).

**SDK snapshot:**

A snapshot test in each SDK against a recorded response from the dev
box keeps the wire shape stable. Generated from a real response, not
a hand-mocked fixture, so the test ages with the server's behavior.

## Out of scope for this design

- **CPU utilization.** The schema, samples table, and API shape all
  support it (mirror `memoryUsedGbSeconds` etc. with `cpuUsedCoreSeconds`,
  `usedCpuPctAvg`, `allocatedCpuPct`). Blocked by `usage_collector.go:103`
  — the collector writes `CPUUsec: 0` with a TODO to parse cgroup
  `cpu.stat`. Follow-up: read `usage_usec` from each sandbox's
  `cpu.stat`, store cumulative; deltas between samples give per-minute
  CPU time consumed. Once samples carry real values, the API gains
  the symmetric CPU fields with no shape change.
- New `/api/usage` sort modes by actual usage. Mentioned as a natural
  follow-up; the leaderboard endpoint design is separate work.
- A dashboard page. Falls out trivially from this endpoint, but the
  rendering work is a UI task, not an API design.
- Network and disk I/O metrics. The `sandbox_usage_samples` schema
  doesn't carry them today, and nobody has asked. If they're needed
  later, the schema extends and the points gain new fields — no shape
  change.
- Sub-minute resolution. Tied to the collector's 60s cadence; would
  require changing the sample rate (and significantly more storage).
- Aggregating across all of a customer's sandboxes ("show me my org
  over time"). Belongs in a future `/api/usage?groupBy=time` variant,
  not on the per-sandbox endpoint.
