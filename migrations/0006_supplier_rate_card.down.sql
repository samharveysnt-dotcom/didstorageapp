BEGIN;
ALTER TABLE rate_cards DROP CONSTRAINT IF EXISTS rate_cards_supplier_bill_inc_check;
ALTER TABLE rate_cards DROP CONSTRAINT IF EXISTS rate_cards_supplier_bill_min_check;
ALTER TABLE rate_cards
  DROP COLUMN IF EXISTS supplier_bill_increment_seconds,
  DROP COLUMN IF EXISTS supplier_bill_min_seconds,
  DROP COLUMN IF EXISTS supplier_mrc_cents,
  DROP COLUMN IF EXISTS supplier_nrc_cents,
  DROP COLUMN IF EXISTS supplier_per_minute_cents;
COMMIT;
