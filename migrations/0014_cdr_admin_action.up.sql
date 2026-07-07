-- Mark CDRs whose call was terminated by an admin live-action.
--
-- Admin hangup / warn / redirect from /live writes a "pending admin
-- action" key into Redis at action time. When /sipctl/cdr fires after the
-- channel actually ends, it pops that key and stamps these columns on the
-- CDR insert. Result: the /cdrs row carries enough context to be obviously
-- distinguishable from a normal caller-initiated hangup.
--
-- We reuse the existing user_block_action enum (added in 0013) so the
-- values stay aligned with the compliance log:
--   live_hangup  / live_warn / live_redirect
-- Other enum members (block, unblock) won't appear here.

ALTER TABLE cdrs
    ADD COLUMN IF NOT EXISTS admin_action        user_block_action NULL,
    ADD COLUMN IF NOT EXISTS admin_action_by     BIGINT NULL REFERENCES admins(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS admin_action_reason TEXT   NULL;

CREATE INDEX IF NOT EXISTS cdrs_admin_action_idx
    ON cdrs (admin_action, started_at DESC)
    WHERE admin_action IS NOT NULL;
