BEGIN;
DROP TABLE IF EXISTS api_keys, sessions, admins, cdrs, billing_runs, balance_ledger,
                     did_assignments, sip_accounts, users, pops, resellers, dids,
                     rate_cards, supplier_ip_group_members, supplier_ip_groups,
                     suppliers, countries CASCADE;
DROP TYPE  IF EXISTS ledger_kind, assignment_status, route_kind, did_type;
COMMIT;
