-- Seed four CDRs representing each call scenario so we can visually verify
-- the trace stack — including admin-action markers — without placing real
-- SIP calls. Run as: sudo -u postgres psql didstorage -f seed_trace_scenarios.sql
--
-- Each row gets:
--   - a realistic siptrace_json blob (~5-10 SIP messages with sane endpoints
--     and timing)
--   - the existing order/user/supplier/DID FK chain (assumes order #1 with
--     user #1 and supplier #1 already exist — true on the running box)
--   - for admin-actioned scenarios: a matching user_block_log entry so the
--     trace page's marker query (loadAdminActionEvents) returns it
--
-- Idempotent: re-running deletes the seeded rows by call_id before inserting.

BEGIN;

DELETE FROM user_block_log WHERE reason LIKE 'SEED:%';
DELETE FROM cdrs WHERE call_id LIKE 'seed-%';

-- ─────────────────────────────────────────────────────────────────────
-- Scenario 1: NORMAL ANSWERED CALL
-- Supplier dials a DID, peer answers, talk for 12s, peer hangs up.
-- No admin action — baseline reference.
-- ─────────────────────────────────────────────────────────────────────
INSERT INTO cdrs (
    call_id, order_id, user_id, supplier_id, did_id, pop_id,
    routed_kind, routed_target,
    started_at, answered_at, ended_at, billsec, charged_minutes,
    rate_cents_per_min, charge_cents,
    supplier_charge_cents, supplier_bill_min_seconds, supplier_bill_increment_seconds,
    hangup_cause, src_uri, dst_uri,
    siptrace_json, siptrace_computed_at
)
SELECT
    'seed-normal-001', 1, 1, 1, 1, NULL,
    'sip_uri', 'sip:904@mouselike.org',
    '2026-05-11 12:00:00+00', '2026-05-11 12:00:03+00', '2026-05-11 12:00:15+00',
    12, 1, 2.5, 3, 1, 60, 60,
    '16', '447956816884', '442038968001',
    $$
    {
      "call_id": "seed-normal-001",
      "endpoints": ["217.73.68.35:5060", "45.8.93.244:5060", "5.6.7.8:5060"],
      "final_sip_code": 200,
      "final_sip_reason": "OK",
      "method_counts": {"INVITE": 2, "ACK": 2, "BYE": 2, "200": 4, "180": 2, "100": 2},
      "messages": [
        {"unix_time": 1778155200.000, "time": "2026-05-11T12:00:00.000Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "INVITE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155200.050, "time": "2026-05-11T12:00:00.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155200.080, "time": "2026-05-11T12:00:00.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "INVITE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155200.130, "time": "2026-05-11T12:00:00.130Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155201.200, "time": "2026-05-11T12:00:01.200Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155201.250, "time": "2026-05-11T12:00:01.250Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155203.000, "time": "2026-05-11T12:00:03.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155203.050, "time": "2026-05-11T12:00:03.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155203.100, "time": "2026-05-11T12:00:03.100Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "ACK sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155203.150, "time": "2026-05-11T12:00:03.150Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "ACK sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155215.000, "time": "2026-05-11T12:00:15.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "BYE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155215.040, "time": "2026-05-11T12:00:15.040Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155215.080, "time": "2026-05-11T12:00:15.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "BYE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155215.120, "time": "2026-05-11T12:00:15.120Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"}
      ],
      "raw": ""
    }
    $$::jsonb,
    now();

-- ─────────────────────────────────────────────────────────────────────
-- Scenario 2: ADMIN HANGUP mid-call (after 8 seconds of talk).
-- Caller is on the line; admin clicks Hangup from /live — Asterisk
-- sends BYE upstream + downstream. Compliance log entry seeded too.
-- ─────────────────────────────────────────────────────────────────────
INSERT INTO cdrs (
    call_id, order_id, user_id, supplier_id, did_id, pop_id,
    routed_kind, routed_target,
    started_at, answered_at, ended_at, billsec, charged_minutes,
    rate_cents_per_min, charge_cents,
    supplier_charge_cents, supplier_bill_min_seconds, supplier_bill_increment_seconds,
    hangup_cause, src_uri, dst_uri,
    admin_action, admin_action_by, admin_action_reason,
    siptrace_json, siptrace_computed_at
)
SELECT
    'seed-hangup-002', 1, 1, 1, 1, NULL,
    'sip_uri', 'sip:904@mouselike.org',
    '2026-05-11 12:05:00+00', '2026-05-11 12:05:03+00', '2026-05-11 12:05:11+00',
    8, 1, 2.5, 3, 1, 60, 60,
    '16', '447956816884', '442038968001',
    'live_hangup'::user_block_action, 1, 'SEED: suspected fraud — terminated mid-call',
    $$
    {
      "call_id": "seed-hangup-002",
      "endpoints": ["217.73.68.35:5060", "45.8.93.244:5060", "5.6.7.8:5060"],
      "final_sip_code": 200,
      "final_sip_reason": "OK",
      "method_counts": {"INVITE": 2, "ACK": 2, "BYE": 2, "200": 4, "180": 2, "100": 2},
      "messages": [
        {"unix_time": 1778155500.000, "time": "2026-05-11T12:05:00.000Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "INVITE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155500.050, "time": "2026-05-11T12:05:00.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155500.080, "time": "2026-05-11T12:05:00.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "INVITE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155500.140, "time": "2026-05-11T12:05:00.140Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155501.500, "time": "2026-05-11T12:05:01.500Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155501.550, "time": "2026-05-11T12:05:01.550Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155503.000, "time": "2026-05-11T12:05:03.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155503.050, "time": "2026-05-11T12:05:03.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155503.100, "time": "2026-05-11T12:05:03.100Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "ACK sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155503.150, "time": "2026-05-11T12:05:03.150Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "ACK sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155511.000, "time": "2026-05-11T12:05:11.000Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "BYE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155511.020, "time": "2026-05-11T12:05:11.020Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "BYE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155511.080, "time": "2026-05-11T12:05:11.080Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155511.110, "time": "2026-05-11T12:05:11.110Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"}
      ],
      "raw": ""
    }
    $$::jsonb,
    now();

INSERT INTO user_block_log (user_id, action, reason, order_id, blocked_by, details, created_at)
VALUES (1, 'live_hangup', 'SEED: suspected fraud — terminated mid-call', 1, 1,
        'call_id=seed-hangup-002 channel=PJSIP/globetelecom-seed', '2026-05-11 12:05:10.900+00');

-- ─────────────────────────────────────────────────────────────────────
-- Scenario 3: ADMIN WARN mid-call. Channel transfers to admin-actions,
-- answers (already answered in this case), plays a prompt, hangs up.
-- ─────────────────────────────────────────────────────────────────────
INSERT INTO cdrs (
    call_id, order_id, user_id, supplier_id, did_id, pop_id,
    routed_kind, routed_target,
    started_at, answered_at, ended_at, billsec, charged_minutes,
    rate_cents_per_min, charge_cents,
    supplier_charge_cents, supplier_bill_min_seconds, supplier_bill_increment_seconds,
    hangup_cause, src_uri, dst_uri,
    admin_action, admin_action_by, admin_action_reason,
    siptrace_json, siptrace_computed_at
)
SELECT
    'seed-warn-003', 1, 1, 1, 1, NULL,
    'sip_uri', 'sip:904@mouselike.org',
    '2026-05-11 12:10:00+00', '2026-05-11 12:10:03+00', '2026-05-11 12:10:18+00',
    15, 1, 2.5, 3, 1, 60, 60,
    '16', '447956816884', '442038968001',
    'live_warn'::user_block_action, 1, 'SEED: prompt=im-sorry · flagged caller list match',
    $$
    {
      "call_id": "seed-warn-003",
      "endpoints": ["217.73.68.35:5060", "45.8.93.244:5060", "5.6.7.8:5060"],
      "final_sip_code": 200,
      "final_sip_reason": "OK",
      "method_counts": {"INVITE": 2, "ACK": 2, "BYE": 2, "200": 4, "180": 2, "100": 2},
      "messages": [
        {"unix_time": 1778155800.000, "time": "2026-05-11T12:10:00.000Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "INVITE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155800.050, "time": "2026-05-11T12:10:00.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155800.080, "time": "2026-05-11T12:10:00.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "INVITE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155800.130, "time": "2026-05-11T12:10:00.130Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778155801.000, "time": "2026-05-11T12:10:01.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155801.050, "time": "2026-05-11T12:10:01.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778155803.000, "time": "2026-05-11T12:10:03.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155803.050, "time": "2026-05-11T12:10:03.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155803.100, "time": "2026-05-11T12:10:03.100Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "ACK sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155803.150, "time": "2026-05-11T12:10:03.150Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "ACK sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155811.000, "time": "2026-05-11T12:10:11.000Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "BYE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778155811.050, "time": "2026-05-11T12:10:11.050Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778155818.000, "time": "2026-05-11T12:10:18.000Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "BYE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778155818.040, "time": "2026-05-11T12:10:18.040Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"}
      ],
      "raw": ""
    }
    $$::jsonb,
    now();

-- Two compliance entries for the warn — one for the action itself, one
-- mirroring the literal moment the audio prompt finished and the BYE went
-- out. Lets the user see the "warn → prompt → hangup" sequence on the trace.
INSERT INTO user_block_log (user_id, action, reason, order_id, blocked_by, details, created_at)
VALUES
  (1, 'live_warn', 'SEED: prompt=im-sorry · flagged caller list match', 1, 1,
   'call_id=seed-warn-003 prompt=im-sorry', '2026-05-11 12:10:10.900+00');

-- ─────────────────────────────────────────────────────────────────────
-- Scenario 4: ADMIN REDIRECT. Drop the existing outbound leg, bridge the
-- caller to a new destination (sip:fraud-honeypot@example.com).
-- ─────────────────────────────────────────────────────────────────────
INSERT INTO cdrs (
    call_id, order_id, user_id, supplier_id, did_id, pop_id,
    routed_kind, routed_target,
    started_at, answered_at, ended_at, billsec, charged_minutes,
    rate_cents_per_min, charge_cents,
    supplier_charge_cents, supplier_bill_min_seconds, supplier_bill_increment_seconds,
    hangup_cause, src_uri, dst_uri,
    admin_action, admin_action_by, admin_action_reason,
    siptrace_json, siptrace_computed_at
)
SELECT
    'seed-redirect-004', 1, 1, 1, 1, NULL,
    'sip_uri', 'sip:904@mouselike.org',
    '2026-05-11 12:15:00+00', '2026-05-11 12:15:03+00', '2026-05-11 12:15:35+00',
    32, 1, 2.5, 3, 1, 60, 60,
    '16', '447956816884', '442038968001',
    'live_redirect'::user_block_action, 1, 'SEED: route=sip_uri:sip:fraud-honeypot@example.com · diverting to honeypot',
    $$
    {
      "call_id": "seed-redirect-004",
      "endpoints": ["217.73.68.35:5060", "45.8.93.244:5060", "5.6.7.8:5060", "10.99.0.42:5060"],
      "final_sip_code": 200,
      "final_sip_reason": "OK",
      "method_counts": {"INVITE": 3, "ACK": 3, "BYE": 3, "200": 6, "180": 3, "100": 3},
      "messages": [
        {"unix_time": 1778156100.000, "time": "2026-05-11T12:15:00.000Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "INVITE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778156100.050, "time": "2026-05-11T12:15:00.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778156100.080, "time": "2026-05-11T12:15:00.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "INVITE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778156100.140, "time": "2026-05-11T12:15:00.140Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778156101.000, "time": "2026-05-11T12:15:01.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778156101.050, "time": "2026-05-11T12:15:01.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778156103.000, "time": "2026-05-11T12:15:03.000Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778156103.050, "time": "2026-05-11T12:15:03.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778156103.100, "time": "2026-05-11T12:15:03.100Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "ACK sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778156103.150, "time": "2026-05-11T12:15:03.150Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "ACK sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778156113.000, "time": "2026-05-11T12:15:13.000Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "5.6.7.8:5060",       "summary": "BYE sip:904@mouselike.org SIP/2.0"},
        {"unix_time": 1778156113.050, "time": "2026-05-11T12:15:13.050Z", "direction": "in",  "src_addr": "5.6.7.8:5060",       "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778156113.200, "time": "2026-05-11T12:15:13.200Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "10.99.0.42:5060",  "summary": "INVITE sip:fraud-honeypot@example.com SIP/2.0"},
        {"unix_time": 1778156113.260, "time": "2026-05-11T12:15:13.260Z", "direction": "in",  "src_addr": "10.99.0.42:5060",  "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 100 Trying"},
        {"unix_time": 1778156114.000, "time": "2026-05-11T12:15:14.000Z", "direction": "in",  "src_addr": "10.99.0.42:5060",  "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 180 Ringing"},
        {"unix_time": 1778156115.000, "time": "2026-05-11T12:15:15.000Z", "direction": "in",  "src_addr": "10.99.0.42:5060",  "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778156115.050, "time": "2026-05-11T12:15:15.050Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "10.99.0.42:5060",  "summary": "ACK sip:fraud-honeypot@example.com SIP/2.0"},
        {"unix_time": 1778156135.000, "time": "2026-05-11T12:15:35.000Z", "direction": "in",  "src_addr": "217.73.68.35:5060", "dst_addr": "45.8.93.244:5060", "summary": "BYE sip:442038968001@45.8.93.244 SIP/2.0"},
        {"unix_time": 1778156135.040, "time": "2026-05-11T12:15:35.040Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "217.73.68.35:5060", "summary": "SIP/2.0 200 OK"},
        {"unix_time": 1778156135.080, "time": "2026-05-11T12:15:35.080Z", "direction": "out", "src_addr": "45.8.93.244:5060", "dst_addr": "10.99.0.42:5060",  "summary": "BYE sip:fraud-honeypot@example.com SIP/2.0"},
        {"unix_time": 1778156135.130, "time": "2026-05-11T12:15:35.130Z", "direction": "in",  "src_addr": "10.99.0.42:5060",  "dst_addr": "45.8.93.244:5060", "summary": "SIP/2.0 200 OK"}
      ],
      "raw": ""
    }
    $$::jsonb,
    now();

INSERT INTO user_block_log (user_id, action, reason, order_id, blocked_by, details, created_at)
VALUES (1, 'live_redirect',
        'SEED: route=sip_uri:sip:fraud-honeypot@example.com · diverting to honeypot',
        1, 1,
        'call_id=seed-redirect-004 route=sip_uri:sip:fraud-honeypot@example.com',
        '2026-05-11 12:15:13.100+00');

COMMIT;

\echo
\echo '=== seeded CDRs ==='
SELECT call_id, billsec, hangup_cause, admin_action::text, started_at
  FROM cdrs WHERE call_id LIKE 'seed-%' ORDER BY started_at;
\echo
\echo '=== seeded compliance events ==='
SELECT to_char(created_at,'HH24:MI:SS') AS at, action::text, order_id, substring(reason FROM 1 FOR 60)
  FROM user_block_log WHERE reason LIKE 'SEED:%' ORDER BY created_at;
\echo
\echo 'Visit:'
\echo '  /cdrs                                       — all four with badge differentiation'
\echo '  /cdrs/seed-normal-001/sip-trace             — baseline answered call'
\echo '  /cdrs/seed-hangup-002/sip-trace             — admin hangup marker mid-call'
\echo '  /cdrs/seed-warn-003/sip-trace               — admin warn marker before BYE'
\echo '  /cdrs/seed-redirect-004/sip-trace           — admin redirect marker between original BYE and new INVITE'
\echo '  /orders/1#cdrs                              — same CDRs under the order'
\echo '  /orders/1#compliance                        — the three SEED entries'
