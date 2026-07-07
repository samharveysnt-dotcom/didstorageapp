BEGIN;
ALTER TABLE cdrs
  DROP COLUMN IF EXISTS supplier_bill_increment_seconds,
  DROP COLUMN IF EXISTS supplier_bill_min_seconds,
  DROP COLUMN IF EXISTS supplier_charge_cents;
DROP INDEX IF EXISTS rate_cards_active_uq;
COMMIT;
