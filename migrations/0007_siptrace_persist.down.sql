BEGIN;
DROP INDEX IF EXISTS cdrs_siptrace_pending_idx;
ALTER TABLE cdrs
  DROP COLUMN IF EXISTS siptrace_computed_at,
  DROP COLUMN IF EXISTS siptrace_json;
COMMIT;
