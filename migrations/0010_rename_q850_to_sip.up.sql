-- Phase 8: rename the 'q850' cause family to 'sip' (these codes show up
-- in our SIP stack traces; the legacy ITU naming was confusing). Title-Case
-- every built-in label and switch the detail prefix from "Q.850 / N — "
-- to "SIPN — " so admins reading the tooltip see the same identifier the
-- /cause-codes page filters on.

BEGIN;

-- 1) Drop the old constraint so we can update family values without it
--    rejecting rows that don't match the OLD enum.
ALTER TABLE hangup_causes DROP CONSTRAINT IF EXISTS hangup_causes_family_check;

-- 2) Re-prefix the detail strings on the q850 rows. Done before the family
--    rename so we can target precisely (idempotent — already-converted rows
--    don't match the regex).
UPDATE hangup_causes
   SET detail = 'SIP' || code || ' — ' ||
                regexp_replace(detail, '^Q\.850\s*/\s*\d+\s*[—-]\s*', '')
 WHERE family = 'q850';

-- 3) Family rename.
UPDATE hangup_causes SET family = 'sip' WHERE family = 'q850';

-- 4) Re-add the constraint with the new allowed values now that no rows
--    violate it.
ALTER TABLE hangup_causes ADD CONSTRAINT hangup_causes_family_check
  CHECK (family IN ('sip','platform'));

-- 5) Title-case + acronym-safe labels for every built-in row. VALUES list
--    so non-builtin admin edits aren't clobbered.
UPDATE hangup_causes hc SET label = v.label, updated_at = now()
  FROM (VALUES
    ('1',   'Unallocated Number'),
    ('16',  'Normal'),
    ('17',  'User Busy'),
    ('18',  'No User Response'),
    ('19',  'No Answer'),
    ('20',  'Subscriber Absent'),
    ('21',  'Call Rejected'),
    ('22',  'Number Changed'),
    ('27',  'Destination Out Of Order'),
    ('28',  'Invalid Number'),
    ('29',  'Facility Rejected'),
    ('31',  'Normal, Unspecified'),
    ('34',  'No Circuit Available'),
    ('38',  'Network Out Of Order'),
    ('41',  'Temporary Failure'),
    ('42',  'Switching Congestion'),
    ('47',  'Resource Unavailable'),
    ('50',  'Facility Not Subscribed'),
    ('57',  'Bearer Not Authorized'),
    ('58',  'Bearer Not Available'),
    ('65',  'Bearer Not Implemented'),
    ('69',  'Facility Not Implemented'),
    ('79',  'Service/Option Not Implemented'),
    ('81',  'Invalid Call Reference'),
    ('88',  'Incompatible Destination'),
    ('95',  'Invalid Message'),
    ('96',  'Mandatory IE Missing'),
    ('97',  'Message Type Not Implemented'),
    ('99',  'IE Not Implemented'),
    ('100', 'Invalid IE Contents'),
    ('102', 'Recovery On Timer Expiry'),
    ('111', 'Protocol Error'),
    ('127', 'Interworking, Unspecified'),
    -- Platform causes — same Title Case treatment; keep all-caps acronyms.
    ('insufficient_channels',     'Channel Cap Reached'),
    ('insufficient_balance',      'Low Balance'),
    ('user_blocked',              'User Blocked'),
    ('quarantined',               'Order Quarantined'),
    ('unknown_did',               'Unknown DID'),
    ('unauthorized_ip',           'Unauthorized Source IP'),
    ('reservation_misconfigured', 'Reservation Misconfigured')
  ) AS v(code, label)
 WHERE hc.code = v.code AND hc.builtin = true;

COMMIT;
