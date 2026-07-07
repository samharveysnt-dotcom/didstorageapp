-- Customer-side billing increment per rate card.
--
-- Until this migration, customer charges were hard-coded to 60/60 (one-minute
-- minimum, one-minute round-up) inside domain.ChargeForCall. Suppliers
-- already had per-rate-card supplier_bill_min_seconds + supplier_bill_increment_seconds
-- (added back in migration 0006). This migration brings the same configurability
-- to the customer side.
--
-- Notation operators use: "min/inc" seconds.
--   60/60  → 60s connection charge, then 60s round-up (same as legacy default)
--   60/1   → 60s connection charge, then per-second
--   60/6   → 60s connection charge, then 6s increments
--   6/6    → 6s connection charge, then 6s increments
--   30/30  → 30s connection charge, then 30s increments
--
-- Both columns default to 60 so the migration is a no-op for billing on
-- existing rate cards. CHECK > 0 prevents zero / negative values that would
-- divide-by-zero downstream.
--
-- The cdrs table gets nullable copies so every CDR carries the increment it
-- was actually billed with. Legacy rows stay NULL; new rows always snapshot.
-- The supplier-side already does this with supplier_bill_min/inc columns.

BEGIN;

ALTER TABLE rate_cards
    ADD COLUMN bill_min_seconds       INTEGER NOT NULL DEFAULT 60
        CHECK (bill_min_seconds > 0 AND bill_min_seconds <= 600),
    ADD COLUMN bill_increment_seconds INTEGER NOT NULL DEFAULT 60
        CHECK (bill_increment_seconds > 0 AND bill_increment_seconds <= 600);

ALTER TABLE cdrs
    ADD COLUMN bill_min_seconds       INTEGER,
    ADD COLUMN bill_increment_seconds INTEGER;

COMMIT;
