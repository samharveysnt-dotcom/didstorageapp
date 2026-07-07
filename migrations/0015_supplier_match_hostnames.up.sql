-- Some carriers (e.g. didcomms) ask the customer to whitelist a SIP hostname
-- rather than (or in addition to) a static IP. PJSIP's `type=identify` accepts
-- a hostname in the `match=` parameter and resolves it to all current A
-- records at config-load time, so storing a hostname lets us follow DNS
-- changes without a code release.
--
-- Schema impact:
--   - `cidr` becomes nullable.
--   - A new `hostname` column carries the DNS name for hostname-style entries.
--   - A CHECK ensures exactly ONE of (cidr, hostname) is set per row, so a
--     single SELECT-and-render pass still has a clear "kind" per row.
--   - The existing UNIQUE (group_id, cidr) still applies to IP rows; a
--     parallel partial unique index covers hostname rows.
--
-- The `/sipctl/authorize` path continues to look up suppliers by source IP
-- against `cidr` only — hostnames are emitted into pjsip_suppliers.conf for
-- PJSIP to expand. PJSIP's resolution happens at the asterisk -rx "pjsip
-- reload" that follows every supplier-IP mutation, so adding a hostname
-- effectively trusts every IP the hostname resolves to right now.

ALTER TABLE supplier_ip_group_members
  ALTER COLUMN cidr DROP NOT NULL;

ALTER TABLE supplier_ip_group_members
  ADD COLUMN hostname text;

ALTER TABLE supplier_ip_group_members
  ADD CONSTRAINT supplier_ip_member_one_of_check
    CHECK ((cidr IS NULL) <> (hostname IS NULL));

CREATE UNIQUE INDEX IF NOT EXISTS supplier_ip_member_hostname_uniq
  ON supplier_ip_group_members (group_id, hostname)
  WHERE hostname IS NOT NULL;

-- Re-grant after schema change — postgres won't auto-extend privileges to
-- the didstorage role for the new column.
GRANT ALL ON TABLE supplier_ip_group_members TO didstorage;
