-- Add a per-sandbox bearer-token gate on preview URLs.
--
-- When preview_auth_hash IS NULL the preview URL is open (current default).
-- With a hash set, the CP's preview-URL proxy (ControlPlaneProxy.doProxy)
-- requires a matching token in `Authorization: Bearer X` or `X-OC-Preview-Token`
-- and 401s otherwise. The check happens at the CP, not the edge, so any
-- entry path that lands at the cell (edge-forwarded, Caddy, direct tunnel,
-- self-hosted) enforces the same gate.
--
-- preview_auth_scheme is split into its own column so future schemes (HMAC,
-- JWT) can land without another migration. Today only "bearer" is supported.

ALTER TABLE sandbox_sessions ADD COLUMN preview_auth_hash TEXT;
ALTER TABLE sandbox_sessions ADD COLUMN preview_auth_scheme TEXT;
