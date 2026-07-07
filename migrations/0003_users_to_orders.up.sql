-- Reframe: an end-user record in our system is conceptually an *order* a
-- reseller places against our platform. We don't host a retail UI; the
-- reseller manages real customer auth in their own system. Each order:
--   - has our internal id (PK)
--   - is owned by exactly one reseller (or NULL for direct/admin-created)
--   - carries an external_id from the reseller's system (their order ref)
--   - has its own balance, channel cap, and DIDs
-- Email + password_hash are removed: orders never log into our platform.

BEGIN;

-- 1. Rename the table.
ALTER TABLE users RENAME TO orders;

-- 2. Drop password requirement; we no longer need it.
ALTER TABLE orders DROP COLUMN password_hash;

-- 3. Email becomes optional contact info, not an identity.
ALTER TABLE orders ALTER COLUMN email DROP NOT NULL;
ALTER TABLE orders RENAME COLUMN email TO contact_email;
-- Drop the constraint first — it auto-drops the backing index.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS users_email_key;
DROP INDEX IF EXISTS users_reseller_idx;
DROP INDEX IF EXISTS users_email_key;

-- 4. Add external_id (reseller's reference) and label (human-readable).
ALTER TABLE orders ADD COLUMN external_id TEXT;
ALTER TABLE orders ADD COLUMN label        TEXT;

-- An external_id is unique per-reseller (different resellers can use the
-- same value in their own systems). NULL is allowed for admin-created
-- orders that have no upstream reseller reference.
CREATE UNIQUE INDEX orders_reseller_external_uq
  ON orders (reseller_id, external_id)
  WHERE external_id IS NOT NULL;

CREATE INDEX orders_reseller_idx ON orders (reseller_id);

-- 5. Rename user_id FK columns across all referencing tables.
ALTER TABLE sip_accounts     RENAME COLUMN user_id TO order_id;
ALTER TABLE did_assignments  RENAME COLUMN user_id TO order_id;
ALTER TABLE balance_ledger   RENAME COLUMN user_id TO order_id;
ALTER TABLE cdrs             RENAME COLUMN user_id TO order_id;

-- 6. Rename indexes that referenced user-named columns.
ALTER INDEX IF EXISTS sip_accounts_user_idx        RENAME TO sip_accounts_order_idx;
ALTER INDEX IF EXISTS did_assignments_user_idx     RENAME TO did_assignments_order_idx;
ALTER INDEX IF EXISTS balance_ledger_user_time_idx RENAME TO balance_ledger_order_time_idx;
ALTER INDEX IF EXISTS cdrs_user_started_idx        RENAME TO cdrs_order_started_idx;

COMMIT;
