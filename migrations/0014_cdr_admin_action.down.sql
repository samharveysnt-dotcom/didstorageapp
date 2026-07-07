DROP INDEX IF EXISTS cdrs_admin_action_idx;
ALTER TABLE cdrs
    DROP COLUMN IF EXISTS admin_action,
    DROP COLUMN IF EXISTS admin_action_by,
    DROP COLUMN IF EXISTS admin_action_reason;
