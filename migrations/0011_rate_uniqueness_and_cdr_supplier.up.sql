-- Phase 9: rate-card uniqueness + per-CDR supplier rate snapshot.
--
-- (A) One ACTIVE rate per (supplier, country, did_type). Historical /
--     expired cards (valid_to IS NOT NULL) stay for accounting; only
--     valid_to IS NULL is enforced unique.
--
-- (B) Snapshot the supplier-side cost on every cdrs row so we can compute
--     profit historically without joining back to rate_cards (which may
--     have been edited or expired since the call).

BEGIN;

CREATE UNIQUE INDEX IF NOT EXISTS rate_cards_active_uq
  ON rate_cards (supplier_id, country_iso, did_type) WHERE valid_to IS NULL;

ALTER TABLE cdrs
  ADD COLUMN IF NOT EXISTS supplier_charge_cents           INTEGER,
  ADD COLUMN IF NOT EXISTS supplier_bill_min_seconds       INTEGER,
  ADD COLUMN IF NOT EXISTS supplier_bill_increment_seconds INTEGER;

COMMIT;
