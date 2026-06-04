-- Persistent FUSE mounts attached to a sandbox.
--
-- Each row is one rclone mount that has been declared `persistent: true`. The
-- worker stores the rendered rclone config (which embeds backend credentials)
-- here, encrypted with the same SECRET_ENCRYPTION_KEY used for the rest of
-- our at-rest secrets. On wake — both explicit and auto-wake — the worker
-- reads this table, decrypts the configs, and replays each mount via the
-- existing mount orchestration.
--
-- Non-persistent mounts (the v1 default) live only in the worker's in-memory
-- registry and do NOT touch this table — they vanish on hibernate by design.
--
-- last_error is set when the wake-time replay fails (bad creds, network, etc).
-- The mount row stays in the table so `mounts.list()` can surface the failure
-- to the user; they remove or re-add via the normal SDK calls.
CREATE TABLE sandbox_mounts (
    sandbox_id TEXT NOT NULL,
    path TEXT NOT NULL,                              -- absolute path inside the VM
    remote TEXT NOT NULL,                            -- rclone remote spec, e.g. "s3:my-bucket/prefix"
    backend TEXT NOT NULL DEFAULT '',                -- "s3" | "gcs" | ... | "" when rcloneConfig supplied raw
    read_only BOOLEAN NOT NULL DEFAULT TRUE,
    mount_options JSONB NOT NULL DEFAULT '[]'::jsonb, -- extra args appended to `rclone mount`
    encrypted_config BYTEA NOT NULL,                 -- nonce || ciphertext of the rclone config string
    last_error TEXT NOT NULL DEFAULT '',             -- set on wake-time replay failure; '' = healthy
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sandbox_id, path)
);

CREATE INDEX sandbox_mounts_sandbox ON sandbox_mounts (sandbox_id);
