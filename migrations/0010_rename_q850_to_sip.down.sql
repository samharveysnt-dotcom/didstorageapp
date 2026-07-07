BEGIN;
-- Revert family rename + detail prefix. Labels stay Title Case (no canonical
-- way back to lowercase that's worth committing to).
UPDATE hangup_causes
   SET detail = 'Q.850 / ' || code || ' — ' ||
                regexp_replace(detail, '^SIP\d+\s*[—-]\s*', '')
 WHERE family = 'sip';

UPDATE hangup_causes SET family = 'q850' WHERE family = 'sip';

ALTER TABLE hangup_causes DROP CONSTRAINT IF EXISTS hangup_causes_family_check;
ALTER TABLE hangup_causes ADD CONSTRAINT hangup_causes_family_check
  CHECK (family IN ('q850','platform'));
COMMIT;
