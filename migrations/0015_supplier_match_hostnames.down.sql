-- Reverses 0015_supplier_match_hostnames. Any hostname-only rows will be
-- deleted first (they have NULL cidr and would violate the NOT NULL we're
-- about to re-apply).

DELETE FROM supplier_ip_group_members WHERE cidr IS NULL;

DROP INDEX IF EXISTS supplier_ip_member_hostname_uniq;

ALTER TABLE supplier_ip_group_members
  DROP CONSTRAINT IF EXISTS supplier_ip_member_one_of_check;

ALTER TABLE supplier_ip_group_members
  DROP COLUMN IF EXISTS hostname;

ALTER TABLE supplier_ip_group_members
  ALTER COLUMN cidr SET NOT NULL;
