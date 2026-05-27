package controlplane

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// PublishLifecycle XADDs a sandbox lifecycle event (`stopped`, `hibernated`,
// `migrated`, etc.) to the cell's events stream. Used for CP-side state
// changes — the scaler's orphan sweep, the maintenance loop's dead-worker
// reconciler — where the worker's per-sandbox SQLite never emitted because
// the worker is gone or never got involved.
//
// Retries up to 3 times with a short backoff so a Redis hiccup doesn't
// permanently drop the event. Drops on this path produce ghost rows in D1
// sandboxes_index that only the periodic reconciler can clean up.
//
// Returns true if the XADD succeeded on some attempt, false if all three
// failed (caller logs/decides next steps).
func PublishLifecycle(ctx context.Context, rdb *redis.Client, cellID, eventType, sandboxID, workerID string, orgID uuid.UUID, reason string) bool {
	if rdb == nil || cellID == "" || sandboxID == "" {
		return false
	}
	envelope := map[string]any{
		"id":         uuid.NewString(),
		"type":       eventType,
		"sandbox_id": sandboxID,
		"org_id":     orgID.String(),
		"worker_id":  workerID,
		"cell_id":    cellID,
		"payload":    map[string]any{"reason": reason},
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("PublishLifecycle marshal %s: %v", eventType, err)
		return false
	}
	stream := "events:" + cellID
	backoff := 200 * time.Millisecond
	for attempt := 1; attempt <= 3; attempt++ {
		xaddCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := rdb.XAdd(xaddCtx, &redis.XAddArgs{
			Stream: stream,
			MaxLen: 100000,
			Approx: true,
			Values: map[string]any{"event": string(body)},
		}).Err()
		cancel()
		if err == nil {
			return true
		}
		log.Printf("PublishLifecycle %s sandbox=%s attempt %d/3 failed: %v", eventType, sandboxID, attempt, err)
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return false
}
