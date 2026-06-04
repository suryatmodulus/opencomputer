package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PersistentMount is one row of sandbox_mounts. The rclone config is stored
// encrypted on disk; callers receive the still-encrypted bytes and decrypt
// via Store.Encryptor() when they actually need to render the config (typically
// during wake-time replay).
type PersistentMount struct {
	SandboxID       string
	Path            string
	Remote          string
	Backend         string
	ReadOnly        bool
	MountOptions    []string
	EncryptedConfig []byte
	LastError       string
}

// UpsertSandboxMount creates or replaces a persistent mount row. Re-issuing
// `mounts.add()` against the same path overwrites the prior spec; this matches
// the in-memory registry's put-with-same-path semantics.
func (s *Store) UpsertSandboxMount(ctx context.Context, m PersistentMount) error {
	opts, err := json.Marshal(m.MountOptions)
	if err != nil {
		return fmt.Errorf("marshal mount options: %w", err)
	}
	if len(opts) == 0 || string(opts) == "null" {
		opts = []byte("[]")
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sandbox_mounts (sandbox_id, path, remote, backend, read_only, mount_options, encrypted_config, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '')
		ON CONFLICT (sandbox_id, path) DO UPDATE SET
			remote           = EXCLUDED.remote,
			backend          = EXCLUDED.backend,
			read_only        = EXCLUDED.read_only,
			mount_options    = EXCLUDED.mount_options,
			encrypted_config = EXCLUDED.encrypted_config,
			last_error       = '',
			updated_at       = now()
	`, m.SandboxID, m.Path, m.Remote, m.Backend, m.ReadOnly, opts, m.EncryptedConfig)
	if err != nil {
		return fmt.Errorf("upsert mount %s/%s: %w", m.SandboxID, m.Path, err)
	}
	return nil
}

// DeleteSandboxMount removes a single mount row by (sandbox_id, path). Returns
// nil even when no row matches — `mounts.remove()` is idempotent.
func (s *Store) DeleteSandboxMount(ctx context.Context, sandboxID, path string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM sandbox_mounts WHERE sandbox_id = $1 AND path = $2
	`, sandboxID, path)
	if err != nil {
		return fmt.Errorf("delete mount %s/%s: %w", sandboxID, path, err)
	}
	return nil
}

// DeleteSandboxMounts wipes every mount row for a sandbox (used on sandbox kill).
func (s *Store) DeleteSandboxMounts(ctx context.Context, sandboxID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sandbox_mounts WHERE sandbox_id = $1`, sandboxID)
	if err != nil {
		return fmt.Errorf("delete mounts for %s: %w", sandboxID, err)
	}
	return nil
}

// ListSandboxMounts returns all persistent mounts attached to a sandbox.
// Returns an empty slice (not nil) when the sandbox has no rows so callers can
// safely range without a nil check.
func (s *Store) ListSandboxMounts(ctx context.Context, sandboxID string) ([]PersistentMount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sandbox_id, path, remote, backend, read_only, mount_options, encrypted_config, last_error
		FROM sandbox_mounts
		WHERE sandbox_id = $1
		ORDER BY path
	`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("list mounts for %s: %w", sandboxID, err)
	}
	defer rows.Close()

	out := []PersistentMount{}
	for rows.Next() {
		var m PersistentMount
		var optsJSON []byte
		if err := rows.Scan(&m.SandboxID, &m.Path, &m.Remote, &m.Backend, &m.ReadOnly, &optsJSON, &m.EncryptedConfig, &m.LastError); err != nil {
			return nil, fmt.Errorf("scan mount row: %w", err)
		}
		if len(optsJSON) > 0 && string(optsJSON) != "null" {
			if err := json.Unmarshal(optsJSON, &m.MountOptions); err != nil {
				return nil, fmt.Errorf("unmarshal mount options for %s/%s: %w", m.SandboxID, m.Path, err)
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("iterate mount rows: %w", err)
	}
	return out, nil
}

// SetSandboxMountError records (or clears) the wake-time replay error for a
// mount. Passing an empty string clears the error, used after a successful
// replay or on a fresh upsert.
func (s *Store) SetSandboxMountError(ctx context.Context, sandboxID, path, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sandbox_mounts
		SET last_error = $3, updated_at = now()
		WHERE sandbox_id = $1 AND path = $2
	`, sandboxID, path, errMsg)
	if err != nil {
		return fmt.Errorf("set mount error for %s/%s: %w", sandboxID, path, err)
	}
	return nil
}
