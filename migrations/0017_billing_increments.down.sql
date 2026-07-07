BEGIN;
ALTER TABLE cdrs       DROP COLUMN IF EXISTS bill_min_seconds;
ALTER TABLE cdrs       DROP COLUMN IF EXISTS bill_increment_seconds;
ALTER TABLE rate_cards DROP COLUMN IF EXISTS bill_min_seconds;
ALTER TABLE rate_cards DROP COLUMN IF EXISTS bill_increment_seconds;
COMMIT;
