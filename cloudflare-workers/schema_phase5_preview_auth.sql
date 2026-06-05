-- Phase 5: preview URL authentication.
--
-- Adds a per-sandbox bearer token check enforced at the edge. The plaintext
-- token is shown once on sandbox create (or rotate); only its SHA-256 hex
-- digest lands here. When preview_auth_hash IS NULL the preview URL is open
-- (current behavior — backwards compatible).
--
-- Scheme is split into its own column so we can add HMAC / JWT verification
-- later without another migration. Today only "bearer" is supported.

ALTER TABLE sandboxes_index ADD COLUMN preview_auth_hash TEXT;
ALTER TABLE sandboxes_index ADD COLUMN preview_auth_scheme TEXT;
