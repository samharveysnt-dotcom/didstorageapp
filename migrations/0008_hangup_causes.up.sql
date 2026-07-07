-- Phase 6: lift hangup-cause labels and tooltip detail strings out of the
-- compiled Go binary into a small admin-editable table. Built-ins (Q.850
-- standard codes + our platform reasons) get builtin=true so the UI hides
-- the delete button for them — admins can edit the wording but not lose
-- the row entirely.

BEGIN;

CREATE TABLE IF NOT EXISTS hangup_causes (
  code       TEXT PRIMARY KEY,
  label      TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT '',
  family     TEXT NOT NULL CHECK (family IN ('q850','platform')) DEFAULT 'q850',
  builtin    BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS hangup_causes_family_idx ON hangup_causes (family, code);

-- Seed Q.850 codes
INSERT INTO hangup_causes (code, label, detail, family, builtin) VALUES
 ('1',   'unallocated number',           'Q.850 / 1 — the called party number is not assigned. The destination doesn''t exist on the receiving switch.', 'q850', true),
 ('16',  'normal',                        'Q.850 / 16 — the call ended normally. Both parties hung up cleanly.', 'q850', true),
 ('17',  'user busy',                     'Q.850 / 17 — the called party is busy on another call.', 'q850', true),
 ('18',  'no user response',              'Q.850 / 18 — the called party did not respond to alerting within the alerting timer.', 'q850', true),
 ('19',  'no answer',                     'Q.850 / 19 — the called party was alerted (rang) but did not answer.', 'q850', true),
 ('20',  'subscriber absent',             'Q.850 / 20 — mobile subscriber is unreachable (off, out of coverage).', 'q850', true),
 ('21',  'call rejected',                 'Q.850 / 21 — the called party rejected the call (e.g. tapped decline).', 'q850', true),
 ('22',  'number changed',                'Q.850 / 22 — the called number has been changed. Diagnostic IE may carry the new number.', 'q850', true),
 ('27',  'destination out of order',      'Q.850 / 27 — the destination is unreachable due to a problem at the far end (e.g. peer down).', 'q850', true),
 ('28',  'invalid number',                'Q.850 / 28 — the called number is incomplete / has invalid format.', 'q850', true),
 ('29',  'facility rejected',             'Q.850 / 29 — a requested facility (CLI display, etc.) was rejected by the network.', 'q850', true),
 ('31',  'normal, unspecified',           'Q.850 / 31 — call ended normally with no specific cause given.', 'q850', true),
 ('34',  'no circuit available',          'Q.850 / 34 — no circuit/channel available to route the call (network congestion).', 'q850', true),
 ('38',  'network out of order',          'Q.850 / 38 — the network is not functioning correctly. Likely a transient infrastructure issue.', 'q850', true),
 ('41',  'temporary failure',             'Q.850 / 41 — short-term failure in the network. Try again.', 'q850', true),
 ('42',  'switching congestion',          'Q.850 / 42 — the network/switch is overloaded.', 'q850', true),
 ('47',  'resource unavailable',          'Q.850 / 47 — generic resource exhaustion at an intermediate switch.', 'q850', true),
 ('50',  'facility not subscribed',       'Q.850 / 50 — the calling user is not subscribed to a requested facility.', 'q850', true),
 ('57',  'bearer not authorized',         'Q.850 / 57 — the calling user is not authorized to use the requested bearer capability.', 'q850', true),
 ('58',  'bearer not available',          'Q.850 / 58 — the requested bearer capability is not currently available.', 'q850', true),
 ('65',  'bearer not implemented',        'Q.850 / 65 — the requested bearer capability is not supported.', 'q850', true),
 ('69',  'facility not implemented',      'Q.850 / 69 — the requested facility is not implemented.', 'q850', true),
 ('79',  'service/option not implemented','Q.850 / 79 — the requested service/option is not implemented at the receiving end.', 'q850', true),
 ('81',  'invalid call reference',        'Q.850 / 81 — the call reference value is not currently in use on the user-network interface.', 'q850', true),
 ('88',  'incompatible destination',      'Q.850 / 88 — destination is not compatible with the requested transit network or bearer.', 'q850', true),
 ('95',  'invalid message',               'Q.850 / 95 — invalid message — generic catch-all for malformed signalling.', 'q850', true),
 ('96',  'mandatory IE missing',          'Q.850 / 96 — a mandatory information element was missing from a message.', 'q850', true),
 ('97',  'message type not implemented',  'Q.850 / 97 — the receiving end does not implement the message type.', 'q850', true),
 ('99',  'IE not implemented',            'Q.850 / 99 — an information element is not implemented at the receiving end.', 'q850', true),
 ('100', 'invalid IE contents',           'Q.850 / 100 — an IE contained illegal content.', 'q850', true),
 ('102', 'recovery on timer expiry',      'Q.850 / 102 — a procedure timed out and the call was cleared in recovery.', 'q850', true),
 ('111', 'protocol error',                'Q.850 / 111 — a generic protocol error occurred.', 'q850', true),
 ('127', 'interworking, unspecified',     'Q.850 / 127 — the cause came from a network using a different signalling protocol.', 'q850', true)
ON CONFLICT (code) DO NOTHING;

-- Seed platform reasons
INSERT INTO hangup_causes (code, label, detail, family, builtin) VALUES
 ('insufficient_channels',     'channel cap reached',         'Platform — the order''s channel_count limit was hit by an in-flight call. Buy more channels or wait for an active call to end.', 'platform', true),
 ('insufficient_balance',      'low balance',                  'Platform — the user''s balance can''t cover the configured minimum call duration. Top up to resume.', 'platform', true),
 ('user_blocked',              'user blocked',                 'Platform — the user account is currently inactive (blocked by an admin). All calls to its DIDs reply 480.', 'platform', true),
 ('quarantined',               'order quarantined',            'Platform — the order is in compliance hold because the parent user is blocked. Calls reply 480.', 'platform', true),
 ('unknown_did',               'unknown DID',                  'Platform — this DID is not in our database. Likely a misconfiguration upstream or attack traffic.', 'platform', true),
 ('unauthorized_ip',           'unauthorized source IP',       'Platform — the source IP is not in any of the matching supplier''s IP groups.', 'platform', true),
 ('reservation_misconfigured', 'reservation misconfigured',    'Platform — the DID is in ''reserved'' state but has no route_target set.', 'platform', true)
ON CONFLICT (code) DO NOTHING;

COMMIT;
