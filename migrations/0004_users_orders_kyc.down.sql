BEGIN;
-- Reverses 0004. Note: assignment_status enum values 'kyc_pending'/'quarantined'
-- cannot be dropped from the enum without recreating the type.

DROP TABLE IF EXISTS denied_calls;
DROP TABLE IF EXISTS user_block_log;
DROP TYPE  IF EXISTS user_block_action;

ALTER TABLE orders DROP COLUMN IF EXISTS pre_quarantine_route_target;
ALTER TABLE orders DROP COLUMN IF EXISTS pre_quarantine_route_kind;
ALTER TABLE orders DROP COLUMN IF EXISTS kyc_bundle_id;

DROP TABLE IF EXISTS kyc_documents;
DROP TABLE IF EXISTS kyc_bundles;
DROP TYPE  IF EXISTS kyc_doc_kind;
DROP TYPE  IF EXISTS kyc_bundle_status;
DROP TYPE  IF EXISTS kyc_bundle_type;

ALTER TABLE resellers DROP COLUMN IF EXISTS brand_name;

ALTER TABLE billing_runs RENAME COLUMN order_id TO did_assignment_id;
ALTER INDEX IF EXISTS billing_runs_order_idx RENAME TO billing_runs_assignment_idx;

ALTER TABLE cdrs RENAME COLUMN order_id TO did_assignment_id;
ALTER TABLE cdrs RENAME COLUMN user_id TO order_id;
ALTER INDEX IF EXISTS cdrs_user_started_idx RENAME TO cdrs_order_started_idx;

ALTER TABLE balance_ledger RENAME COLUMN user_id TO order_id;
ALTER INDEX IF EXISTS balance_ledger_user_time_idx RENAME TO balance_ledger_order_time_idx;

ALTER TABLE sip_accounts RENAME COLUMN user_id TO order_id;
ALTER INDEX IF EXISTS sip_accounts_user_idx RENAME TO sip_accounts_order_idx;

ALTER TABLE orders RENAME COLUMN user_id TO order_id;
ALTER INDEX IF EXISTS orders_did_active_idx RENAME TO did_assignments_did_active_idx;
ALTER INDEX IF EXISTS orders_billing_due_idx RENAME TO did_assignments_billing_due_idx;
ALTER INDEX IF EXISTS orders_one_active_per_did RENAME TO did_assignments_one_active_per_did;
ALTER INDEX IF EXISTS orders_user_idx RENAME TO did_assignments_order_idx;

ALTER TABLE orders RENAME TO did_assignments;
ALTER TABLE users  RENAME TO orders;

COMMIT;
