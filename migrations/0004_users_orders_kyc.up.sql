-- Phase 2 restructure:
--   * Customer-level entity is now "users" (was "orders")
--   * Per-DID rental is now "orders" (was "did_assignments")
--   * KYC bundles + documents (user-owned, admin-approved, attached to orders)
--   * Append-only user block log (compliance trail)
--   * Resellers gain a "brand_name" used for white-label sanitization
--   * Orders pick up: kyc_bundle_id, pre-quarantine route memory, kyc_pending +
--     quarantined statuses
--   * denied_calls table for unauthorized_ip / unknown_did (separate from cdrs
--     so attack traffic doesn't pollute customer-visible CDR lists)

BEGIN;

-- ==================================================================
-- 1. Rename current `orders` (customer-level) -> `users`
-- ==================================================================
ALTER TABLE orders RENAME TO users;

-- ==================================================================
-- 2. Rename `did_assignments` (per-DID rental) -> `orders`
-- ==================================================================
ALTER TABLE did_assignments RENAME TO orders;

ALTER INDEX did_assignments_did_active_idx     RENAME TO orders_did_active_idx;
ALTER INDEX did_assignments_billing_due_idx    RENAME TO orders_billing_due_idx;
ALTER INDEX did_assignments_one_active_per_did RENAME TO orders_one_active_per_did;
ALTER INDEX IF EXISTS did_assignments_order_idx RENAME TO orders_user_idx;

-- The order_id column on this table currently points at the old orders table
-- (now `users`). Rename to user_id to match the new model.
ALTER TABLE orders RENAME COLUMN order_id TO user_id;

-- ==================================================================
-- 3. Rename FK columns in dependent tables
-- ==================================================================
ALTER TABLE sip_accounts    RENAME COLUMN order_id TO user_id;
ALTER INDEX IF EXISTS sip_accounts_order_idx RENAME TO sip_accounts_user_idx;

ALTER TABLE balance_ledger  RENAME COLUMN order_id TO user_id;
ALTER INDEX IF EXISTS balance_ledger_order_time_idx RENAME TO balance_ledger_user_time_idx;

ALTER TABLE cdrs            RENAME COLUMN order_id TO user_id;
ALTER INDEX IF EXISTS cdrs_order_started_idx RENAME TO cdrs_user_started_idx;

-- cdrs.did_assignment_id now references the table called "orders" — rename to
-- order_id for consistency with the new domain language.
ALTER TABLE cdrs            RENAME COLUMN did_assignment_id TO order_id;

-- billing_runs.did_assignment_id likewise.
ALTER TABLE billing_runs    RENAME COLUMN did_assignment_id TO order_id;
ALTER INDEX IF EXISTS billing_runs_assignment_idx RENAME TO billing_runs_order_idx;

-- ==================================================================
-- 4. Extend assignment_status enum with kyc_pending + quarantined
--    (Postgres requires committing the ADD VALUE before using it; we do so
--    within the migration's transaction by using ALTER TYPE ... ADD VALUE
--    which is allowed inside a transaction in PG12+.)
-- ==================================================================
ALTER TYPE assignment_status ADD VALUE IF NOT EXISTS 'kyc_pending';
ALTER TYPE assignment_status ADD VALUE IF NOT EXISTS 'quarantined';

-- ==================================================================
-- 5. Reseller white-label brand name (separate from internal `name`)
-- ==================================================================
ALTER TABLE resellers ADD COLUMN IF NOT EXISTS brand_name TEXT;

-- ==================================================================
-- 6. KYC bundles + documents
-- ==================================================================
CREATE TYPE kyc_bundle_type   AS ENUM ('person', 'company');
CREATE TYPE kyc_bundle_status AS ENUM ('pending', 'approved', 'rejected');
CREATE TYPE kyc_doc_kind      AS ENUM ('id_front', 'id_back', 'passport', 'address_proof', 'company_registration', 'other');

CREATE TABLE kyc_bundles (
  id               BIGSERIAL PRIMARY KEY,
  user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type             kyc_bundle_type   NOT NULL,
  status           kyc_bundle_status NOT NULL DEFAULT 'pending',
  -- legal name, dob, registered address, company number, etc.
  info             JSONB NOT NULL DEFAULT '{}'::jsonb,
  approved_by      BIGINT REFERENCES admins(id),
  approved_at      TIMESTAMPTZ,
  rejected_at      TIMESTAMPTZ,
  rejection_reason TEXT,
  notes            TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX kyc_bundles_user_idx   ON kyc_bundles (user_id);
CREATE INDEX kyc_bundles_status_idx ON kyc_bundles (status) WHERE status = 'pending';

CREATE TABLE kyc_documents (
  id            BIGSERIAL PRIMARY KEY,
  bundle_id     BIGINT NOT NULL REFERENCES kyc_bundles(id) ON DELETE CASCADE,
  kind          kyc_doc_kind NOT NULL,
  filename      TEXT   NOT NULL,
  mime_type     TEXT   NOT NULL,
  size_bytes    BIGINT NOT NULL,
  -- relative to /var/lib/didstorage/kyc/, e.g. "{user_id}/{bundle_id}/{filename}"
  storage_path  TEXT   NOT NULL,
  uploaded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX kyc_documents_bundle_idx ON kyc_documents (bundle_id);

-- ==================================================================
-- 7. Orders pick up KYC link + pre-quarantine route memory
-- ==================================================================
ALTER TABLE orders ADD COLUMN kyc_bundle_id               BIGINT REFERENCES kyc_bundles(id) ON DELETE SET NULL;
ALTER TABLE orders ADD COLUMN pre_quarantine_route_kind   route_kind;
ALTER TABLE orders ADD COLUMN pre_quarantine_route_target TEXT;

-- ==================================================================
-- 8. Append-only user block log (compliance audit trail)
-- ==================================================================
CREATE TYPE user_block_action AS ENUM ('block', 'unblock');

CREATE TABLE user_block_log (
  id             BIGSERIAL PRIMARY KEY,
  user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  action         user_block_action NOT NULL,
  reason         TEXT NOT NULL,
  -- snapshot of which KYC bundle was relevant at block time (may be deleted
  -- later — we keep the bundle id for audit, even if FK no longer resolves).
  kyc_bundle_id  BIGINT,
  blocked_by     BIGINT REFERENCES admins(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX user_block_log_user_idx ON user_block_log (user_id, created_at DESC);

-- ==================================================================
-- 9. denied_calls — separate from cdrs for unauthorized_ip / unknown_did
--    These are typically attack traffic and we don't want them polluting
--    a customer-visible CDR list.
-- ==================================================================
CREATE TABLE denied_calls (
  id          BIGSERIAL PRIMARY KEY,
  call_id     TEXT NOT NULL,
  src_ip      INET NOT NULL,
  to_uri      TEXT NOT NULL,
  from_uri    TEXT,
  reason      TEXT NOT NULL,  -- 'unauthorized_ip' | 'unknown_did' | ...
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX denied_calls_time_idx ON denied_calls (created_at DESC);
CREATE INDEX denied_calls_src_idx  ON denied_calls (src_ip, created_at DESC);
CREATE INDEX denied_calls_reason_idx ON denied_calls (reason, created_at DESC);

COMMIT;
