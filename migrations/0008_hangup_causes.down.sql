BEGIN;
DROP INDEX IF EXISTS hangup_causes_family_idx;
DROP TABLE IF EXISTS hangup_causes;
COMMIT;
