//go:build pgfixture

// Per-sandbox usage-points correctness against real Postgres. Runs only
// under `go test -tags=pgfixture` with TEST_DATABASE_URL set. The pure-
// Go tests in usage_points_test.go cover SQL shape and validation but
// cannot exercise generate_series, the LEAST/GREATEST overlap clamp,
// or the reconciliation invariant against GetOrgUsage. Those live here.
//
// Run locally:
//   TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable \
//     go test -tags=pgfixture ./internal/db/ -run UsagePoints -v
package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedSandboxUsagePointsFixture writes a deterministic set of scale
// events and usage samples for a single sandbox, scoped to a fresh
// org UUID so repeated runs don't leak state.
//
// Layout (t0 = `from`):
//
//	scale_event #1:  [t0-1h, t0+6h30s),   memory_mb=2048
//	scale_event #2:  [t0+6h30s, t0+18h),  memory_mb=4096
//
//	samples Phase A: t0 .. t0+(6h-1m)     360 rows, memory_bytes=1.4 GB
//	samples Phase B: t0+6h .. t0+(8h-1m)  120 rows, memory_bytes=2.8 GB
//	samples GAP:     t0+8h .. t0+(9h-1m)  no rows  (60 missing buckets)
//	samples Phase C: t0+9h .. t0+(18h-1m) 540 rows, memory_bytes=3.5 GB
//	(no samples after t0+18h — sandbox stopped)
//
// This shape exercises: resize event mid-bucket (bucket 360), a gap
// where the scale event is still open (allocation accrues, uptime
// reads 0), and post-stop buckets (both zero).
func seedSandboxUsagePointsFixture(t *testing.T, store *Store, orgID uuid.UUID, sandboxID string, t0 time.Time) {
	t.Helper()
	ctx := context.Background()

	resizeAt := t0.Add(6*time.Hour + 30*time.Second)
	stoppedAt := t0.Add(18 * time.Hour)

	insertSE := func(memMB int, started, ended time.Time) {
		if _, err := store.pool.Exec(ctx,
			`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent, disk_mb, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			sandboxID, orgID, memMB, memMB/40, 20480, started, ended); err != nil {
			t.Fatalf("seed scale event: %v", err)
		}
	}
	insertSamples := func(startMin, endMin int, memMB int, memBytes int64) {
		startTs := t0.Add(time.Duration(startMin) * time.Minute)
		endTs := t0.Add(time.Duration(endMin) * time.Minute)
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO sandbox_usage_samples
			  (sandbox_id, org_id, sampled_at, memory_mb, cpu_usec, memory_bytes, pids)
			SELECT $1, $2, gs.ts, $3, 0, $4, 1
			FROM generate_series($5::timestamptz, $6::timestamptz - interval '1 minute', interval '1 minute') AS gs(ts)
			ON CONFLICT DO NOTHING`,
			sandboxID, orgID, memMB, memBytes, startTs, endTs); err != nil {
			t.Fatalf("seed samples %d..%d: %v", startMin, endMin, err)
		}
	}

	insertSE(2048, t0.Add(-1*time.Hour), resizeAt)
	insertSE(4096, resizeAt, stoppedAt)

	insertSamples(0, 6*60, 2048, 1_400_000_000)        // Phase A
	insertSamples(6*60, 8*60, 4096, 2_800_000_000)     // Phase B
	// Gap: 8*60..9*60 — no samples
	insertSamples(9*60, 18*60, 4096, 3_500_000_000)    // Phase C
}

// TestSandboxUsagePoints_Shape_pgfixture asserts the bucket grid, the
// time-weighted allocation across the resize bucket, the gap-with-open-
// allocation behavior, and the post-stop zero buckets.
func TestSandboxUsagePoints_Shape_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	orgID := uuid.New()
	sandboxID := "sbx-usage-shape"
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	to := t0.Add(24 * time.Hour)
	seedSandboxUsagePointsFixture(t, store, orgID, sandboxID, t0)

	points, totals, err := store.SandboxUsagePoints(ctx, orgID, sandboxID, t0, to)
	if err != nil {
		t.Fatalf("SandboxUsagePoints: %v", err)
	}

	if got, want := len(points), 24*60; got != want {
		t.Fatalf("len(points) = %d, want %d", got, want)
	}

	// Bucket 0 — phase A, sample present.
	p0 := points[0]
	if !p0.Timestamp.Equal(t0) {
		t.Errorf("points[0].Timestamp = %v, want %v", p0.Timestamp, t0)
	}
	if p0.AllocatedMemoryMb != 2048 {
		t.Errorf("points[0].AllocatedMemoryMb = %d, want 2048", p0.AllocatedMemoryMb)
	}
	assertClose(t, "points[0].MemoryAllocatedGbSeconds", 120, p0.MemoryAllocatedGbSeconds)
	if p0.UptimeSeconds != 60 {
		t.Errorf("points[0].UptimeSeconds = %d, want 60", p0.UptimeSeconds)
	}
	assertClose(t, "points[0].MemoryUsedGbSeconds", 84, p0.MemoryUsedGbSeconds)

	// Bucket 360 — resize at t0+6h+30s splits the bucket 50/50 between
	// 2 GiB and 4 GiB tiers. Weighted allocated MB:
	//   (2048 * 30 + 4096 * 30) / 60 = 3072
	// GB-seconds:
	//   (2048/1024) * 30 + (4096/1024) * 30 = 60 + 120 = 180
	p360 := points[360]
	if p360.AllocatedMemoryMb != 3072 {
		t.Errorf("points[360].AllocatedMemoryMb = %d, want 3072 (resize bucket)", p360.AllocatedMemoryMb)
	}
	assertClose(t, "points[360].MemoryAllocatedGbSeconds (resize bucket)", 180, p360.MemoryAllocatedGbSeconds)

	// Bucket 480 — inside the sample gap (t0+8h..t0+9h). Scale event 2
	// is still open here so allocation accrues, but uptime reads 0.
	p480 := points[480]
	if p480.AllocatedMemoryMb != 4096 {
		t.Errorf("points[480].AllocatedMemoryMb = %d, want 4096 (gap with open scale event)", p480.AllocatedMemoryMb)
	}
	assertClose(t, "points[480].MemoryAllocatedGbSeconds (gap)", 240, p480.MemoryAllocatedGbSeconds)
	if p480.UptimeSeconds != 0 {
		t.Errorf("points[480].UptimeSeconds = %d, want 0 (no sample in gap)", p480.UptimeSeconds)
	}
	if p480.UsedMemoryMbAvg != 0 {
		t.Errorf("points[480].UsedMemoryMbAvg = %d, want 0 (no sample in gap)", p480.UsedMemoryMbAvg)
	}

	// Bucket 1080 — after stop (t0+18h). No scale event, no sample.
	p1080 := points[1080]
	if p1080.AllocatedMemoryMb != 0 || p1080.UptimeSeconds != 0 {
		t.Errorf("points[1080] = %+v, want all zeros (post-stop)", p1080)
	}
	assertClose(t, "points[1080].MemoryAllocatedGbSeconds", 0, p1080.MemoryAllocatedGbSeconds)

	// Totals sanity.
	//   Expected allocated_gb_seconds:
	//     event 1 in [t0, t0+6h30s) → 21630s × 2 GiB = 43260
	//     event 2 in [t0+6h30s, t0+18h) → 41370s × 4 GiB = 165480
	//     total = 208740 ... wait, recompute: 18h - 6h30s = 11h59m30s = 43170s
	//                                          43170 * 4 = 172680
	//   Sum = 43260 + 172680 = 215940
	assertClose(t, "totals.MemoryAllocatedGbSeconds", 215940, totals.MemoryAllocatedGbSeconds)
	if totals.MemoryAllocatedPeakMb != 4096 {
		t.Errorf("totals.MemoryAllocatedPeakMb = %d, want 4096", totals.MemoryAllocatedPeakMb)
	}
	if totals.UptimeSeconds != 1020*60 {
		t.Errorf("totals.UptimeSeconds = %d, want %d (1020 sampled minutes)", totals.UptimeSeconds, 1020*60)
	}
}

// TestSandboxUsagePoints_SumInvariant_pgfixture pins the contract that
// Σ points.* == totals.* exactly (Go-side sum, not a separate query),
// for every additive field. If this drifts, the response shape breaks
// composability — a client summing points to reconstruct totals would
// get a different number than the totals field.
func TestSandboxUsagePoints_SumInvariant_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	orgID := uuid.New()
	sandboxID := "sbx-usage-sum-invariant"
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	to := t0.Add(24 * time.Hour)
	seedSandboxUsagePointsFixture(t, store, orgID, sandboxID, t0)

	points, totals, err := store.SandboxUsagePoints(ctx, orgID, sandboxID, t0, to)
	if err != nil {
		t.Fatalf("SandboxUsagePoints: %v", err)
	}

	var sumAlloc, sumUsed float64
	var sumUptime int
	for _, p := range points {
		sumAlloc += p.MemoryAllocatedGbSeconds
		sumUsed += p.MemoryUsedGbSeconds
		sumUptime += p.UptimeSeconds
	}
	assertClose(t, "Σ MemoryAllocatedGbSeconds == totals", totals.MemoryAllocatedGbSeconds, sumAlloc)
	assertClose(t, "Σ MemoryUsedGbSeconds == totals", totals.MemoryUsedGbSeconds, sumUsed)
	if sumUptime != totals.UptimeSeconds {
		t.Errorf("Σ UptimeSeconds = %d, totals = %d", sumUptime, totals.UptimeSeconds)
	}
}

// TestSandboxUsagePoints_Reconciliation_pgfixture pins the load-bearing
// claim that totals.MemoryAllocatedGbSeconds == GetOrgUsage's rolled-up
// allocated total for the same sandbox + window. If this drifts, the
// new endpoint's "what would the bill say" answer disagrees with what
// the Stripe pipeline actually reports — a silent correctness bug.
func TestSandboxUsagePoints_Reconciliation_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	orgID := uuid.New()
	sandboxID := "sbx-usage-reconcile"
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	to := t0.Add(24 * time.Hour)
	seedSandboxUsagePointsFixture(t, store, orgID, sandboxID, t0)

	_, totals, err := store.SandboxUsagePoints(ctx, orgID, sandboxID, t0, to)
	if err != nil {
		t.Fatalf("SandboxUsagePoints: %v", err)
	}

	// GetOrgUsage groups by (memory_mb, cpu_percent, disk_mb). Since we
	// seeded exactly one sandbox, the org-level rollup IS this sandbox's
	// rollup; allocated GB-seconds = SUM(memory_mb/1024 * total_seconds).
	summaries, err := store.GetOrgUsage(ctx, orgID.String(), t0, to)
	if err != nil {
		t.Fatalf("GetOrgUsage: %v", err)
	}
	var rollupAlloc float64
	for _, s := range summaries {
		rollupAlloc += float64(s.MemoryMB) / 1024.0 * s.TotalSeconds
	}
	if rollupAlloc == 0 {
		t.Fatal("expected GetOrgUsage to return non-zero allocated; fixture not seeded?")
	}
	assertClose(t, "totals.MemoryAllocatedGbSeconds vs GetOrgUsage", rollupAlloc, totals.MemoryAllocatedGbSeconds)
}
