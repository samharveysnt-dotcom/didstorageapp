BEGIN;
-- Reverses 0005.

ALTER TABLE user_block_log DROP CONSTRAINT IF EXISTS user_block_log_user_id_fkey;
ALTER TABLE user_block_log ADD CONSTRAINT user_block_log_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT;

ALTER TABLE balance_ledger DROP CONSTRAINT IF EXISTS balance_ledger_user_id_fkey;
ALTER TABLE balance_ledger ADD CONSTRAINT balance_ledger_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id);

ALTER TABLE cdrs DROP CONSTRAINT IF EXISTS cdrs_order_id_fkey;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_did_assignment_id_fkey
  FOREIGN KEY (order_id) REFERENCES orders(id);

ALTER TABLE cdrs DROP CONSTRAINT IF EXISTS cdrs_user_id_fkey;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id);

-- Re-NOT-NULL: only safe if no rows have NULL.
-- ALTER TABLE cdrs ALTER COLUMN order_id SET NOT NULL;
-- ALTER TABLE cdrs ALTER COLUMN user_id  SET NOT NULL;

DROP INDEX IF EXISTS cdrs_did_started_idx2;
ALTER TABLE cdrs DROP COLUMN IF EXISTS routed_target;
ALTER TABLE cdrs DROP COLUMN IF EXISTS routed_kind;
ALTER TABLE cdrs DROP COLUMN IF EXISTS did_id;

ALTER TABLE dids DROP CONSTRAINT IF EXISTS dids_status_check;
ALTER TABLE dids ADD CONSTRAINT dids_status_check
  CHECK (status = ANY (ARRAY['available'::text, 'assigned'::text, 'retired'::text]));

ALTER TABLE dids DROP COLUMN IF EXISTS reserved_note;
ALTER TABLE dids DROP COLUMN IF EXISTS reserved_by;
ALTER TABLE dids DROP COLUMN IF EXISTS reserved_at;
ALTER TABLE dids DROP COLUMN IF EXISTS reserved_route_target;
ALTER TABLE dids DROP COLUMN IF EXISTS reserved_route_kind;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_channel_count_check;
ALTER TABLE orders ADD CONSTRAINT did_assignments_channel_count_check CHECK (channel_count >= 2);

ALTER TABLE dids ALTER COLUMN supplier_channel_cap DROP NOT NULL;
ALTER TABLE dids ALTER COLUMN supplier_channel_cap DROP DEFAULT;

ALTER TABLE users ALTER COLUMN global_channel_cap DROP NOT NULL;
ALTER TABLE users ALTER COLUMN global_channel_cap DROP DEFAULT;

COMMIT;
