-- Reverse 040_add_updated_at: drop triggers, columns, and the shared function.

DROP TRIGGER IF EXISTS users_set_updated_at                  ON users;
DROP TRIGGER IF EXISTS api_keys_set_updated_at               ON api_keys;
DROP TRIGGER IF EXISTS org_memberships_set_updated_at        ON org_memberships;
DROP TRIGGER IF EXISTS templates_set_updated_at              ON templates;
DROP TRIGGER IF EXISTS sandbox_sessions_set_updated_at       ON sandbox_sessions;
DROP TRIGGER IF EXISTS sandbox_checkpoints_set_updated_at    ON sandbox_checkpoints;
DROP TRIGGER IF EXISTS org_subscription_items_set_updated_at ON org_subscription_items;

ALTER TABLE users                  DROP COLUMN IF EXISTS updated_at;
ALTER TABLE api_keys               DROP COLUMN IF EXISTS updated_at;
ALTER TABLE org_memberships        DROP COLUMN IF EXISTS updated_at;
ALTER TABLE templates              DROP COLUMN IF EXISTS updated_at;
ALTER TABLE sandbox_sessions       DROP COLUMN IF EXISTS updated_at;
ALTER TABLE sandbox_checkpoints    DROP COLUMN IF EXISTS updated_at;
ALTER TABLE org_subscription_items DROP COLUMN IF EXISTS updated_at;

DROP FUNCTION IF EXISTS set_updated_at();
