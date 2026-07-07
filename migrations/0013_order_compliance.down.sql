-- Revert order-scoping + live-action audit fields on user_block_log.
-- The enum values can't be dropped (Postgres won't remove ADDed enum
-- members), so a true revert requires DROP TYPE + CREATE TYPE + UPDATE
-- existing rows — left as a manual exercise if needed.

DROP INDEX IF EXISTS user_block_log_order_idx;
ALTER TABLE user_block_log DROP COLUMN IF EXISTS order_id;
ALTER TABLE user_block_log DROP COLUMN IF EXISTS details;
