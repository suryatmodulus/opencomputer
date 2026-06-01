package controlplane

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// ReconcileStoppedOnWorker re-issues DestroySandbox for any sandbox the cell
// believes is stopped on this worker but the worker may still be hosting.
//
// Why this exists:
//   When the worker is unreachable, the customer's DELETE goes through the
//   cell-side fallback at internal/api/sandbox.go's destroy handler — the
//   cell marks the session stopped in PG and publishes a `stopped` lifecycle
//   event "to close the drift" with D1. Worker never receives the gRPC
//   Destroy. When the worker becomes reachable again, the cell already
//   thinks the sandbox is dead, but the worker still has m.vms[id] alive,
//   qemu still running, usage_ticker still emitting `usage_tick` events.
//   That window has run for 74h+ in the wild before a worker restart finally
//   cleared the m.vms map.
//
// This reconcile closes that gap: on worker rejoin (RedisWorkerRegistry's
// OnWorkerRejoined callback), the cell sweeps every sandbox it has marked
// stopped on that worker within the last 24h and re-issues DestroySandbox.
// The RPC is idempotent — manager.Kill returns "sandbox not found" cleanly
// for entries the worker doesn't have, so genuine first-seen workers (empty
// m.vms) are a cheap no-op.
//
// Paired with: Fix A/B/C in internal/qemu/ghost_reaper.go which catches the
// case where qemu died but m.vms wasn't cleaned. This function catches the
// symmetric case where the cell already declared the sandbox dead but the
// worker (with live qemu) doesn't know.
func ReconcileStoppedOnWorker(ctx context.Context, registry *RedisWorkerRegistry, store *db.Store, workerID string) {
	if store == nil || registry == nil {
		return
	}

	ids, err := store.ListSandboxIDsByWorkerStatus(ctx, workerID, "stopped")
	if err != nil {
		log.Printf("controlplane: reconcile %s: list stopped sessions failed: %v", workerID, err)
		return
	}
	if len(ids) == 0 {
		return
	}

	client, err := registry.GetWorkerClient(workerID)
	if err != nil {
		// Worker was just added to the registry by handleHeartbeat; if the
		// gRPC dial is still in flight (or just failed) we'll catch the
		// next rejoin. Don't retry inline — that'd block other rejoins.
		log.Printf("controlplane: reconcile %s: GetWorkerClient failed: %v", workerID, err)
		return
	}

	log.Printf("controlplane: reconcile %s: sweeping %d cell-stopped sandboxes", workerID, len(ids))

	var killed, notFound, errored int
	for _, id := range ids {
		rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.DestroySandbox(rpcCtx, &pb.DestroySandboxRequest{SandboxId: id})
		cancel()
		if err != nil {
			// "sandbox not found" is the success-case: the worker didn't
			// have it (either a genuine new worker or the entry was cleaned
			// up another way). Only log+count non-not-found errors.
			if isSandboxNotFound(err) {
				notFound++
			} else {
				log.Printf("controlplane: reconcile %s: DestroySandbox %s failed: %v", workerID, id, err)
				errored++
			}
			continue
		}
		killed++
	}
	log.Printf("controlplane: reconcile %s: done — killed=%d not_found=%d errored=%d (of %d cell-stopped within 24h)",
		workerID, killed, notFound, errored, len(ids))
}

// isSandboxNotFound is a heuristic for the worker's "sandbox not found" error
// surface — the qemu/firecracker managers both return `fmt.Errorf("sandbox %s
// not found", id)` from Kill on unknown IDs, which propagates through the
// gRPC layer as an Unknown error containing that string. We treat that as the
// success case for reconcile: nothing to clean up.
func isSandboxNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "sandbox not found") ||
		strings.Contains(err.Error(), "not found")
}
