-- Phase 5: persist parsed SIP traces alongside the CDR so the /sip-trace
-- page is instant for completed calls. tshark fully parses each pcap
-- (multi-GB) which routinely takes 15+ seconds. Pre-computing once at
-- call-end and stashing the JSON beats re-parsing every time an admin
-- opens the trace.

BEGIN;
ALTER TABLE cdrs
  ADD COLUMN IF NOT EXISTS siptrace_json        JSONB,
  ADD COLUMN IF NOT EXISTS siptrace_computed_at TIMESTAMPTZ;
-- Cheap partial index for the "needs precompute" maintenance query.
CREATE INDEX IF NOT EXISTS cdrs_siptrace_pending_idx
  ON cdrs (id) WHERE siptrace_computed_at IS NULL;
COMMIT;
