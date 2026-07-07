-- Phase 3 polish:
--   * Channel-cap convention: -1 = uncapped (was NULL); SET NOT NULL.
--   * Drop the channel_count >= 2 floor; allow 0 for testing.
--   * DID reservations: temporary admin-set route on a DID (no user/order).
--   * cdrs route snapshot: did_id + routed_kind + routed_target so the
--     /dids/{id}/cdrs view can show what the call was routed to *at the
--     time*, even after the order's route has been edited.
--   * Loosen FK actions on cdrs / balance_ledger / user_block_log so an
--     admin can fully delete a user (we cancel orders explicitly to keep
--     the DID return path clean; cdrs become unlinked instead of blocking
--     the delete).

BEGIN;

-- ==================================================================
-- 1. -1 sentinel for "uncapped" channel caps
-- ==================================================================
UPDATE users SET global_channel_cap = -1 WHERE global_channel_cap IS NULL;
ALTER TABLE users ALTER COLUMN global_channel_cap SET DEFAULT -1;
ALTER TABLE users ALTER COLUMN global_channel_cap SET NOT NULL;

UPDATE dids SET supplier_channel_cap = -1 WHERE supplier_channel_cap IS NULL;
ALTER TABLE dids ALTER COLUMN supplier_channel_cap SET DEFAULT -1;
ALTER TABLE dids ALTER COLUMN supplier_channel_cap SET NOT NULL;

-- ==================================================================
-- 2. orders.channel_count >= 0 (was >= 2)
-- ==================================================================
ALTER TABLE orders DROP CONSTRAINT IF EXISTS did_assignments_channel_count_check;
ALTER TABLE orders ADD CONSTRAINT orders_channel_count_check CHECK (channel_count >= 0);

-- ==================================================================
-- 3. DID reservations
-- ==================================================================
ALTER TABLE dids ADD COLUMN IF NOT EXISTS reserved_route_kind   route_kind;
ALTER TABLE dids ADD COLUMN IF NOT EXISTS reserved_route_target TEXT;
ALTER TABLE dids ADD COLUMN IF NOT EXISTS reserved_at           TIMESTAMPTZ;
ALTER TABLE dids ADD COLUMN IF NOT EXISTS reserved_by           BIGINT REFERENCES admins(id);
ALTER TABLE dids ADD COLUMN IF NOT EXISTS reserved_note         TEXT;

ALTER TABLE dids DROP CONSTRAINT IF EXISTS dids_status_check;
ALTER TABLE dids ADD CONSTRAINT dids_status_check
  CHECK (status = ANY (ARRAY['available'::text, 'assigned'::text, 'retired'::text, 'reserved'::text]));

-- ==================================================================
-- 4. CDR route snapshot — preserve which DID and where it routed at the
--    time of the call, so /dids/{id}/cdrs and /users/{id}/cdrs.csv
--    show accurate historical info.
-- ==================================================================
ALTER TABLE cdrs ADD COLUMN IF NOT EXISTS did_id        BIGINT REFERENCES dids(id) ON DELETE SET NULL;
ALTER TABLE cdrs ADD COLUMN IF NOT EXISTS routed_kind   route_kind;
ALTER TABLE cdrs ADD COLUMN IF NOT EXISTS routed_target TEXT;

-- Backfill did_id for existing rows by walking through the order link.
UPDATE cdrs c SET did_id = (SELECT o.did_id FROM orders o WHERE o.id = c.order_id)
 WHERE did_id IS NULL AND order_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS cdrs_did_started_idx2 ON cdrs (did_id, started_at DESC);

-- ==================================================================
-- 5. Soften FK actions so an admin "delete user" can succeed without
--    erasing the audit history of cdrs that referenced them.
-- ==================================================================
ALTER TABLE cdrs ALTER COLUMN user_id  DROP NOT NULL;
ALTER TABLE cdrs ALTER COLUMN order_id DROP NOT NULL;

ALTER TABLE cdrs DROP CONSTRAINT IF EXISTS cdrs_user_id_fkey;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE cdrs DROP CONSTRAINT IF EXISTS cdrs_did_assignment_id_fkey;
ALTER TABLE cdrs DROP CONSTRAINT IF EXISTS cdrs_order_id_fkey;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_order_id_fkey
  FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE SET NULL;

ALTER TABLE balance_ledger DROP CONSTRAINT IF EXISTS balance_ledger_user_id_fkey;
ALTER TABLE balance_ledger ADD CONSTRAINT balance_ledger_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE user_block_log DROP CONSTRAINT IF EXISTS user_block_log_user_id_fkey;
ALTER TABLE user_block_log ADD CONSTRAINT user_block_log_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

-- orders.user_id stays ON DELETE RESTRICT — the userDelete handler is
-- responsible for cancelling orders (which returns DIDs to the pool)
-- before deleting the user, so we keep this guard in place.

COMMIT;
