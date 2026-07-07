-- Compliance-log extensions for order-scoped events + live-call actions.
--
-- Three concerns rolled into one migration so they stay coupled:
--   1. user_block_action got 3 new variants (live_hangup, live_warn,
--      live_redirect) — admin actions from the /live page now flow through
--      the same audit table as block/unblock.
--   2. user_block_log gets an order_id column (nullable) so each compliance
--      event can be scoped to a specific order in addition to the user.
--      Existing block/unblock rows leave it NULL — they're user-wide.
--   3. A details column for action-specific payload (prompt name on warn,
--      redirect target on redirect, etc.). Stored as text — small enough
--      to not justify JSONB and we want it to render verbatim in the log.

ALTER TYPE user_block_action ADD VALUE IF NOT EXISTS 'live_hangup';
ALTER TYPE user_block_action ADD VALUE IF NOT EXISTS 'live_warn';
ALTER TYPE user_block_action ADD VALUE IF NOT EXISTS 'live_redirect';

ALTER TABLE user_block_log
    ADD COLUMN IF NOT EXISTS order_id BIGINT REFERENCES orders(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS details  TEXT;

CREATE INDEX IF NOT EXISTS user_block_log_order_idx
    ON user_block_log (order_id, created_at DESC)
    WHERE order_id IS NOT NULL;

-- New tables don't auto-grant; this is the same pattern as 0012.
-- ALTER TABLE adds don't need re-granting on existing rows but the index
-- creation is fine without grants.
