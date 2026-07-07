-- Phase 4: extend rate_cards with the supplier-side cost picture so admins
-- can see margin (what we charge − what the supplier charges us). Also adds
-- the supplier's billing increment (1/1, 6/6, 30/30, 60/60) since that
-- materially affects per-call cost estimation.

BEGIN;

ALTER TABLE rate_cards
  ADD COLUMN IF NOT EXISTS supplier_per_minute_cents NUMERIC(10,4) NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS supplier_nrc_cents        INTEGER       NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS supplier_mrc_cents        INTEGER       NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS supplier_bill_min_seconds INTEGER       NOT NULL DEFAULT 60,
  ADD COLUMN IF NOT EXISTS supplier_bill_increment_seconds INTEGER NOT NULL DEFAULT 60;

-- Supplier billing increments are conventionally given as "min/inc" pairs.
-- Constrain to common operator values (1, 6, 15, 30, 60 seconds).
ALTER TABLE rate_cards
  DROP CONSTRAINT IF EXISTS rate_cards_supplier_bill_min_check,
  ADD  CONSTRAINT rate_cards_supplier_bill_min_check
    CHECK (supplier_bill_min_seconds IN (1, 6, 15, 30, 60));
ALTER TABLE rate_cards
  DROP CONSTRAINT IF EXISTS rate_cards_supplier_bill_inc_check,
  ADD  CONSTRAINT rate_cards_supplier_bill_inc_check
    CHECK (supplier_bill_increment_seconds IN (1, 6, 15, 30, 60));

COMMIT;
