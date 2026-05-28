package controlplane

import (
	"context"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/opensandbox/opensandbox/internal/cellevents"
)

// PublishLifecycle is preserved here as a re-export for existing callers
// (scaler, cmd/server). The implementation lives in internal/cellevents so
// the worker can publish without pulling in the controlplane graph.
func PublishLifecycle(ctx context.Context, rdb *redis.Client, cellID, eventType, sandboxID, workerID string, orgID uuid.UUID, reason string) bool {
	return cellevents.PublishLifecycle(ctx, rdb, cellID, eventType, sandboxID, workerID, orgID, reason)
}
