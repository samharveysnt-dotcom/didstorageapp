// Package sipctl is the HTTP control plane Asterisk talks to on every call.
//
// /authorize is called on every INVITE: we look up the DID, verify the supplier
// IP, find the active order, reserve a channel in Redis, compute the maximum
// allowed call duration from the user's balance, and return the route.
//
// /cdr is called on dialog teardown: we compute the call's charge, debit the
// user's balance ledger, write a CDR row, and release the channel reservation.
//
// Domain note (post 0004 migration):
//   user  = the customer (balance lives here, channel cap, kyc bundles)
//   order = a DID rental for a user (one DID per order, has its own MRC/NRC,
//           route, anniversary)
//
// Denial taxonomy (matches what we surface in the admin GUI):
//   insufficient_channels  → CDR row,        cause='insufficient_channels'
//   insufficient_balance   → CDR row,        cause='insufficient_balance'
//   user_blocked           → CDR row,        cause='user_blocked'
//   quarantined            → CDR row,        cause='quarantined' (480 reply)
//   unauthorized_ip        → denied_calls,   reason='unauthorized_ip'
//   unknown_did            → denied_calls,   reason='unknown_did'
//
// Call-ID sanitization: cdrs.call_id stores the prefix only (no "@host:port")
// so the customer-visible CDR list doesn't leak supplier addresses. The full
// form is still used for Redis channel reservations and for tshark SIP trace
// lookups (the trace package matches by prefix using a regex).
package sipctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"didstorage/internal/audiogroup"
	"didstorage/internal/db"
	"didstorage/internal/domain"
	"didstorage/internal/livecalls"
	"didstorage/internal/settings"
	"didstorage/internal/siptrace"
)

// hostResolveCache accumulates the set of IPs ever seen via DNS for each
// supplier hostname, and is consulted by supplierIPAllowed. Big-name DID
// providers (DIDLogic among them) use GeoDNS / round-robin and return a
// DIFFERENT subset of POP IPs on every query — a naive "current resolution
// IS the allowed set" cache misses POPs that didn't happen to be in this
// query's response. We accumulate instead: every refresh adds new IPs to
// the hostname's set and bumps their last-seen timestamp; individual IPs
// expire after entryTTL of going un-observed (catches POPs decommissioned
// upstream). PJSIP itself does something analogous on `pjsip reload`.
//
// Triggering: lookups happen on-demand from supplierIPAllowed, but only
// once per refreshTTL per hostname (a successful match short-circuits, so
// the cost is bounded). Cold start: first INVITE for a hostname incurs a
// single ~1.5s-timeout DNS query.
var (
	hostResolveCache   = map[string]*hostResolveEntry{}
	hostResolveCacheMu sync.RWMutex
)

const (
	// How often we re-query DNS for a given hostname. Match PJSIP's
	// reload-driven cadence loosely — too short = DNS hammer, too long
	// = missed new POPs.
	hostRefreshTTL = 60 * time.Second
	// How long an IP can go un-observed before we drop it. Long enough
	// to ride out any single carrier's round-robin cycle; short enough
	// that a truly decommissioned IP doesn't keep auth'ing for weeks.
	hostEntryTTL = 24 * time.Hour
)

type hostResolveEntry struct {
	// IP → last time we saw this IP in a DNS response for the hostname.
	ips map[string]time.Time
	// Last successful DNS query for the hostname (whether or not it
	// changed `ips`). Used to gate refresh frequency.
	lastQueried time.Time
}

// resolveHostCached returns every IP currently considered valid for the
// hostname (union over the last hostEntryTTL of DNS observations). The
// caller should range over the result and compare with srcIP. A refresh
// is triggered if the entry is older than hostRefreshTTL.
//
// Concurrency: read path takes RLock and returns a copy. Refresh path
// takes Lock, re-resolves WITHOUT holding the lock (DNS can be slow),
// then merges + prunes under Lock. The "without holding the lock" detail
// means two simultaneous refreshes can race, but they'll only do redundant
// DNS work — both merge into the same entry and last-write-wins is safe
// because we union rather than replace.
func resolveHostCached(hostname string) []net.IP {
	now := time.Now()

	// Fast path: read-locked snapshot.
	hostResolveCacheMu.RLock()
	if e, ok := hostResolveCache[hostname]; ok && now.Sub(e.lastQueried) < hostRefreshTTL {
		out := snapshotFresh(e.ips, now)
		hostResolveCacheMu.RUnlock()
		return out
	}
	hostResolveCacheMu.RUnlock()

	// Slow path: re-resolve outside any lock. Hard 1.5s timeout so a
	// slow nameserver can't pin authorize latency above what Asterisk's
	// AGI socket is willing to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)

	hostResolveCacheMu.Lock()
	defer hostResolveCacheMu.Unlock()
	e, ok := hostResolveCache[hostname]
	if !ok {
		e = &hostResolveEntry{ips: map[string]time.Time{}}
		hostResolveCache[hostname] = e
	}
	e.lastQueried = now
	if err == nil {
		for _, a := range addrs {
			e.ips[a.IP.String()] = now
		}
	}
	// Prune fully-aged IPs while we hold the write lock.
	for s, t := range e.ips {
		if now.Sub(t) > hostEntryTTL {
			delete(e.ips, s)
		}
	}
	return snapshotFresh(e.ips, now)
}

// snapshotFresh returns a freshly-allocated []net.IP containing every IP
// in `m` whose last-seen ts is within hostEntryTTL of now. Does not
// mutate `m`, so safe to call under either RLock or Lock.
func snapshotFresh(m map[string]time.Time, now time.Time) []net.IP {
	out := make([]net.IP, 0, len(m))
	for s, t := range m {
		if now.Sub(t) > hostEntryTTL {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// PublicIP is set by main once the platform's outward-facing IP is known.
// We need it inside background goroutines (precomputeTrace) which don't have
// the request context to fetch it from. Thread-safe because it's set once
// at startup before any goroutines spawn.
var publicIPForBackground string

// SetPublicIP wires the public IP into the sipctl package so background trace
// pre-computation knows which lane is "ours" for direction labelling.
func SetPublicIP(ip string) { publicIPForBackground = ip }

type Handler struct {
	DB        *db.DB
	Redis     *redis.Client
	AuthToken string
	Log       *slog.Logger

	// MinSecondsToAuthorize: deny INVITEs if balance can't cover this many seconds.
	MinSecondsToAuthorize int
}

// precomputeTrace runs siptrace.Lookup in the background a few seconds after
// a call ends, then stores the JSON result on the cdrs row. Subsequent loads
// of /cdrs/{call_id}/sip-trace are then instant — no tshark needed.
//
// Best-effort: errors are logged and dropped. We delay 5s before lookup so
// the rolling pcap file has had time to flush the call's last packets.
func (h *Handler) precomputeTrace(callID string) {
	go func(cid string) {
		// Sleep with a fresh context, NOT inheriting the request context
		// (which has long since been cancelled).
		select {
		case <-time.After(5 * time.Second):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tr, err := siptrace.Lookup(ctx, cid, publicIPForBackground)
		if err != nil {
			h.Log.Warn("precomputeTrace lookup", "err", err, "call_id", cid)
			return
		}
		// Don't persist an empty trace. A zero-message result usually means
		// something transient (tshark missing, pcap flushing mid-parse, a
		// permission race with sip-capture's daily rotation). Writing it to
		// siptrace_json would let every future page load hit the fast-path
		// and return empty forever. Skipping the UPDATE keeps siptrace_json
		// NULL so /sip-trace re-runs Lookup on the next visit and self-heals.
		if len(tr.Messages) == 0 {
			h.Log.Info("siptrace precompute produced no messages, not persisting",
				"call_id", cid)
			return
		}
		blob, err := json.Marshal(tr)
		if err != nil {
			h.Log.Warn("precomputeTrace marshal", "err", err, "call_id", cid)
			return
		}
		if _, err := h.DB.Exec(ctx,
			`UPDATE cdrs SET siptrace_json = $1::jsonb, siptrace_computed_at = now() WHERE call_id = $2`,
			blob, cid); err != nil {
			h.Log.Warn("precomputeTrace persist", "err", err, "call_id", cid)
			return
		}
		h.Log.Info("siptrace precomputed",
			"call_id", cid, "messages", len(tr.Messages),
			"endpoints", len(tr.Endpoints))
	}(callID)
}

type AuthorizeRequest struct {
	CallID  string `json:"call_id"`
	SrcIP   string `json:"src_ip"`
	ToURI   string `json:"to_uri"`
	FromURI string `json:"from_uri"`
	// AsteriskChannel is the dialplan's ${CHANNEL} at INVITE time —
	// e.g. "PJSIP/globetelecom-00000063". Plumbed in so /live admin
	// actions can hang up / transfer the exact channel without having
	// to grep `pjsip show channels` by Exten (which changes when the
	// channel is transferred for a redirect or warn).
	AsteriskChannel string `json:"asterisk_channel,omitempty"`
}

type AuthorizeResponse struct {
	Decision        string  `json:"decision"`
	Reason          string  `json:"reason,omitempty"`
	// SIPCode is an explicit SIP response code the dialplan should send back.
	// Only set on deny; 0 means "use the dialplan default for this reason".
	// 480 is used for quarantined orders to differentiate them from 403 / 404.
	SIPCode         int     `json:"sip_code,omitempty"`
	// HangupCause is the Q.850 cause the dialplan passes to Hangup() on a
	// deny. Asterisk's standard Q.850→SIP mapping turns this into the
	// appropriate response code (1→404 Not Found, 17→486 Busy Here,
	// 21→403 Forbidden, 31→480 Temporarily Unavailable, 38→502 Bad
	// Gateway, etc.). Picking a cause that matches the actual denial
	// reason gives upstream carriers a useful signal and often controls
	// which fallback prompt they play back to the caller — e.g. "the
	// number you have dialled is not in service" for 404 vs. the
	// longer "sorry we could not connect your call" for 403.
	HangupCause     int     `json:"hangup_cause,omitempty"`
	ReservationID   string  `json:"reservation_id,omitempty"`
	MaxSeconds      int     `json:"max_seconds,omitempty"`
	RouteKind       string  `json:"route_kind,omitempty"`
	RouteTarget     string  `json:"route_target,omitempty"`
	POPID           int64   `json:"pop_id,omitempty"`
	RateCentsPerMin float64 `json:"rate_cents_per_min,omitempty"`
}

type CDRRequest struct {
	CallID        string `json:"call_id"`
	ReservationID string `json:"reservation_id"`
	StartedAt     int64  `json:"started_at"`
	AnsweredAt    int64  `json:"answered_at"`
	EndedAt       int64  `json:"ended_at"`
	Billsec       int    `json:"billsec"`
	HangupCause   string `json:"hangup_cause"`
	SrcURI        string `json:"src_uri"`
	DstURI        string `json:"dst_uri"`
}

type CDRResponse struct {
	Status      string `json:"status"`
	ChargeCents int    `json:"charge_cents"`
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /authorize", h.requireAuth(h.authorize))
	mux.HandleFunc("POST /cdr", h.requireAuth(h.cdr))
}

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-DIDS-Auth") != h.AuthToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	var req AuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, AuthorizeResponse{Decision: "deny", Reason: "bad_json"})
		return
	}
	resp := h.decide(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}

// denialCDR records an authorize-time denial that we attribute to a known
// customer (insufficient_channels, insufficient_balance, user_blocked,
// quarantined). One row per denial so it shows up in the customer's /cdrs
// alongside real calls. call_id is sanitized to its prefix.
//
// We ALSO mirror the denial into denied_calls so the security/abuse view
// surfaces every rejected INVITE in one place, regardless of whether the
// denial was customer-attributable or anonymous attack traffic.
func (h *Handler) denialCDR(ctx context.Context, req AuthorizeRequest, orderID, userID, supplierID, didID int64, ratePerMin float64, cause, routedKind, routedTarget string) {
	defer h.deniedCall(ctx, req, cause)
	now := time.Now().UTC()
	var rkPtr, rtPtr any
	if routedKind != "" {
		rkPtr = routedKind
	}
	if routedTarget != "" {
		rtPtr = routedTarget
	}
	_, err := h.DB.Exec(ctx, `
		INSERT INTO cdrs (call_id, order_id, user_id, supplier_id, did_id,
		                  routed_kind, routed_target,
		                  started_at, answered_at, ended_at, billsec, charged_minutes,
		                  rate_cents_per_min, charge_cents, hangup_cause, src_uri, dst_uri)
		VALUES ($1,$2,$3,$4,$5, $6::route_kind, $7,
		        $8, NULL, $8, 0, 0, $9, 0, $10, $11, $12)
		ON CONFLICT (call_id) DO NOTHING
	`, domain.SanitizeCallID(req.CallID), orderID, userID, supplierID, didID,
		rkPtr, rtPtr,
		now, ratePerMin, cause, req.FromURI, req.ToURI)
	if err != nil {
		h.Log.Warn("denialCDR insert failed", "err", err, "call_id", req.CallID, "cause", cause)
		return
	}
	// Background-precompute the trace so /sip-trace is instant on first open.
	// Even denial CDRs have a few packets in pcap (the inbound INVITE plus
	// our 4xx reply).
	h.precomputeTrace(domain.SanitizeCallID(req.CallID))
}

// supplierIPAllowed returns true if srcIP is inside one of the supplier's IP
// groups. We pulled this out of decide() so the reserved-DID path can ACL
// up front without doubling code.
// supplierIPAllowed reports whether srcIP is whitelisted for supplierID. A
// match exists if srcIP is inside any of the supplier's cidr ranges OR if
// srcIP appears in the resolved-IP set of any of the supplier's hostname
// entries. PJSIP does the same resolution at `pjsip reload` time, so this
// step is what keeps /sipctl/authorize from disagreeing with PJSIP and
// denying a call PJSIP just identified.
//
// Why two passes: a bare-cidr-only EXISTS query is one round trip and
// fits 99% of suppliers (whitelisted by IP). The DNS pass only kicks in
// when the cidr check missed, and is fronted by a 60s in-process cache
// (see resolveHostCached) so we don't hammer DNS on every INVITE.
func (h *Handler) supplierIPAllowed(ctx context.Context, supplierID int64, srcIP net.IP) bool {
	// Pass 1: cidr match. Cheap, indexable, the common case.
	var cidrMatch bool
	err := h.DB.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM supplier_ip_group_members m
		    JOIN supplier_ip_groups g ON g.id = m.group_id
		   WHERE g.supplier_id = $1
		     AND m.cidr IS NOT NULL
		     AND m.cidr >>= $2::inet
		)`, supplierID, srcIP.String()).Scan(&cidrMatch)
	if err != nil {
		h.Log.Error("supplier IP ACL (cidr) failed", "err", err, "supplier", supplierID)
		return false
	}
	if cidrMatch {
		return true
	}

	// Pass 2: hostname match. Pull every hostname for this supplier and
	// see whether any resolves to srcIP. Order doesn't matter — we short-
	// circuit on first hit.
	rows, err := h.DB.Query(ctx, `
		SELECT m.hostname
		  FROM supplier_ip_group_members m
		  JOIN supplier_ip_groups g ON g.id = m.group_id
		 WHERE g.supplier_id = $1
		   AND m.hostname IS NOT NULL`, supplierID)
	if err != nil {
		h.Log.Error("supplier IP ACL (hostname) failed", "err", err, "supplier", supplierID)
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			h.Log.Warn("supplier IP ACL hostname scan", "err", err)
			continue
		}
		for _, ip := range resolveHostCached(host) {
			if ip.Equal(srcIP) {
				return true
			}
		}
	}
	return false
}

// pickAudioGroupMember is a thin wrapper around audiogroup.PickMember
// kept on the Handler so the existing call sites (h.pickAudioGroupMember
// in /sipctl/authorize) keep working without churn. New callers should
// prefer audiogroup.PickMember directly.
func (h *Handler) pickAudioGroupMember(ctx context.Context, groupID int64) (string, error) {
	return audiogroup.PickMember(ctx, h.DB, h.Redis, groupID)
}

// unbilledCDR records a CDR for a DID we own but where there is no order /
// user / rate to bill against. Two cases use this:
//
//  1. Reserved-DID allow: admin staging route. We want a row in /cdrs so
//     every call to one of our DIDs is auditable, with charge=0 and no
//     user/order linkage.
//  2. DID-not-assigned deny: the DID exists in our `dids` table but has
//     status != 'assigned' or has no active order. We deny the call AND
//     record a zero-charge CDR row. (We still mirror to denied_calls in
//     parallel for the abuse view.)
//
// supplier_id is required (NOT NULL on cdrs); did_id, order_id, user_id are
// nullable. cause becomes hangup_cause so the row renders meaningfully on
// /cdrs ("reserved", "did_not_assigned", etc.). started_at and ended_at are
// both set to now() at insert time; /cdr will UPDATE billsec/answered_at/
// ended_at/hangup_cause when the call actually tears down.
func (h *Handler) unbilledCDR(ctx context.Context, req AuthorizeRequest, supplierID, didID int64, cause, routedKind, routedTarget string) {
	now := time.Now().UTC()
	var rkPtr, rtPtr any
	if routedKind != "" {
		rkPtr = routedKind
	}
	if routedTarget != "" {
		rtPtr = routedTarget
	}
	_, err := h.DB.Exec(ctx, `
		INSERT INTO cdrs (call_id, supplier_id, did_id,
		                  routed_kind, routed_target,
		                  started_at, answered_at, ended_at, billsec, charged_minutes,
		                  rate_cents_per_min, charge_cents, hangup_cause, src_uri, dst_uri)
		VALUES ($1,$2,$3, $4::route_kind,$5,
		        $6, NULL, $6, 0, 0, 0, 0, $7, $8, $9)
		ON CONFLICT (call_id) DO NOTHING
	`, domain.SanitizeCallID(req.CallID), supplierID, didID,
		rkPtr, rtPtr,
		now, cause, req.FromURI, req.ToURI)
	if err != nil {
		h.Log.Warn("unbilledCDR insert failed", "err", err, "call_id", req.CallID, "cause", cause)
		return
	}
	h.precomputeTrace(domain.SanitizeCallID(req.CallID))
}

// fillUnbilledCDR is /cdr's tear-down counterpart to unbilledCDR / denialCDR.
// When a call ends and no order resolves (reserved DID, did_not_assigned,
// quarantined denial, etc.), the cdrs row was already inserted at /authorize
// time with billsec=0 and a placeholder ended_at. Here we update the timing
// and duration so the row reflects what actually happened on the wire. We
// never adjust supplier_id/did_id/charge — those snapshots stay frozen at
// authorize-time. hangup_cause is overwritten only if the call answered
// (otherwise the original cause like "did_not_assigned" stays).
func (h *Handler) fillUnbilledCDR(ctx context.Context, callID string, req CDRRequest) {
	if req.Billsec <= 0 && req.AnsweredAt == 0 && req.EndedAt == 0 {
		return
	}
	startedAt := time.Unix(req.StartedAt, 0).UTC()
	endedAt := time.Unix(req.EndedAt, 0).UTC()
	var answeredAt *time.Time
	if req.AnsweredAt > 0 {
		t := time.Unix(req.AnsweredAt, 0).UTC()
		answeredAt = &t
	}
	// Pop pending admin action (live_hangup / live_warn / live_redirect)
	// before the UPDATE so we can stamp it in the same statement. Same
	// flow as the normal /cdr insert path uses.
	var adminAction *string
	var adminActionBy *int64
	var adminActionReason *string
	if pa, perr := livecalls.PopPendingAction(ctx, h.Redis, callID); perr == nil && pa != nil {
		adminAction = &pa.Action
		if pa.AdminID > 0 {
			adminActionBy = &pa.AdminID
		}
		if pa.Reason != "" {
			adminActionReason = &pa.Reason
		}
	}
	// Only overwrite hangup_cause when we actually have a useful one (the
	// authorize-time placeholder is more informative than "" for denials).
	tag, err := h.DB.Exec(ctx, `
		UPDATE cdrs
		   SET started_at   = LEAST(started_at, $2),
		       answered_at  = $3,
		       ended_at     = $4,
		       billsec      = $5,
		       hangup_cause = COALESCE(NULLIF($6,''), hangup_cause),
		       src_uri      = COALESCE(NULLIF($7,''), src_uri),
		       dst_uri      = COALESCE(NULLIF($8,''), dst_uri),
		       admin_action        = COALESCE($9::user_block_action, admin_action),
		       admin_action_by     = COALESCE($10, admin_action_by),
		       admin_action_reason = COALESCE($11, admin_action_reason)
		 WHERE call_id = $1
	`, callID, startedAt, answeredAt, endedAt, req.Billsec, req.HangupCause, req.SrcURI, req.DstURI,
		adminAction, adminActionBy, adminActionReason)
	if err != nil {
		h.Log.Warn("fillUnbilledCDR update failed", "err", err, "call_id", callID)
		return
	}
	if tag.RowsAffected() == 0 {
		// /cdr fired but neither /authorize nor an earlier code path wrote a
		// shell row. Most likely cause: order was cancelled between authorize
		// (active) and /cdr — a real billable call we can no longer charge
		// for. Log loudly so ops can investigate.
		h.Log.Warn("cdr orphan — no matching shell row to update",
			"call_id", callID, "billsec", req.Billsec)
	}
}

// deniedCall records denials we can't attribute to a customer (unauthorized_ip,
// unknown_did). Goes to a separate table so attack traffic doesn't bloat the
// customer-visible CDR list. denied_calls.call_id keeps the raw form (admin
// view only — useful for cross-referencing pcaps when chasing an attacker).
func (h *Handler) deniedCall(ctx context.Context, req AuthorizeRequest, reason string) {
	srcIP := req.SrcIP
	if net.ParseIP(srcIP) == nil {
		srcIP = "0.0.0.0"
	}
	_, err := h.DB.Exec(ctx, `
		INSERT INTO denied_calls (call_id, src_ip, to_uri, from_uri, reason)
		VALUES ($1, $2::inet, $3, $4, $5)
	`, req.CallID, srcIP, req.ToURI, req.FromURI, reason)
	if err != nil {
		h.Log.Warn("deniedCall insert failed", "err", err, "call_id", req.CallID, "reason", reason)
	}
}

// causeForReason maps a deny reason to the Q.850 cause the dialplan hangs
// up with. We deliberately collapse every denial to cause 17 (User Busy →
// SIP 486 Busy Here): upstream carriers translate 486 to a clean busy
// tone (beep-beep) on the PSTN side rather than a recorded prompt like
// "sorry we could not connect your call". The caller hears a familiar
// telephony signal, didcomms doesn't burn per-second prompt time, and
// nothing on either side gets billed for a fake answer.
//
// The internal deny reason string is still preserved on the
// AuthorizeResponse and in the structured log line so diagnostics in
// /cdrs and the journal stay distinguishable — only the on-the-wire SIP
// code is unified. If a future requirement needs reason-specific upstream
// behaviour (e.g. a security policy needing 403 for unauthorized_ip),
// re-branch this switch.
func causeForReason(_ string) int {
	return 17 // User busy → SIP 486 Busy Here → busy tone, no prompt
}

func (h *Handler) decide(ctx context.Context, req AuthorizeRequest) AuthorizeResponse {
	deny := func(reason string) AuthorizeResponse {
		h.Log.Info("authorize deny", "call_id", req.CallID, "reason", reason, "src_ip", req.SrcIP)
		return AuthorizeResponse{
			Decision:    "deny",
			Reason:      reason,
			HangupCause: causeForReason(reason),
		}
	}

	e164 := parseE164(req.ToURI)
	if e164 == "" {
		h.deniedCall(ctx, req, "bad_to_uri")
		return deny("bad_to_uri")
	}
	srcIP := net.ParseIP(req.SrcIP)
	if srcIP == nil {
		h.deniedCall(ctx, req, "bad_src_ip")
		return deny("bad_src_ip")
	}

	// 1. Look up DID first, then optionally the order/user/rate-card.
	var (
		didID, supplierID, userID, orderID, rateCardID int64
		channelCount                                   int
		userBalance                                    int64
		userChannelCap                                 int // -1 = uncapped
		routeKind, routeTarget                         string
		popID                                          *int64
		ratePerMin                                     float64
		userStatus, orderStatus, didStatus             string
	)
	// 1a. Always look up the DID first so we know the supplier (for ACL) and
	// can detect 'reserved' status before joining the orders table.
	var (
		reservedRouteKind, reservedRouteTarget string
		reservedAudioGroupID                   *int64
	)
	err := h.DB.QueryRow(ctx, `
		SELECT id, supplier_id, status,
		       COALESCE(reserved_route_kind::text,''), COALESCE(reserved_route_target,''),
		       reserved_audio_group_id
		  FROM dids WHERE e164 = $1
	`, e164).Scan(&didID, &supplierID, &didStatus, &reservedRouteKind, &reservedRouteTarget, &reservedAudioGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		h.deniedCall(ctx, req, "unknown_did")
		return deny("unknown_did")
	}
	if err != nil {
		h.Log.Error("authorize did lookup failed", "err", err, "call_id", req.CallID)
		return deny("lookup_error")
	}

	// 1b. Verify the supplier-IP ACL up front — applies equally to assigned
	// and reserved DIDs. We don't want random IPs hitting reserved DIDs.
	if !h.supplierIPAllowed(ctx, supplierID, srcIP) {
		h.deniedCall(ctx, req, "unauthorized_ip")
		return deny("src_ip_not_authorized")
	}

	// 1c. Reserved DID short-circuit. No user, no order, no billing — just
	// hand back the admin-supplied route. Used for testing or staging. We
	// still write a zero-charge CDR shell so the call is auditable in /cdrs;
	// /cdr will UPDATE billsec/timing on teardown.
	//
	// reserved_route_kind ∈ (sip_uri | ip | sip_account | audio). The first
	// three are dialled to an outbound target as normal; 'audio' tells the
	// dialplan to answer the call, play the admin-uploaded clip whose
	// basename is encoded in reserved_route_target ("didstorage/af_..."),
	// then hang up. didapi just passes the kind+target through — no audio
	// lookup happens at INVITE time because the basename was baked into
	// reserved_route_target at reservation time (see didReserve).
	if didStatus == "reserved" {
		if reservedRouteKind == "" {
			h.Log.Warn("reserved DID has no route", "did", e164)
			return deny("reservation_misconfigured")
		}
		// audio_group reservations resolve at INVITE time to a single audio
		// file by random-no-repeat (excluding whatever played most recently
		// for the group). The CDR + /live page record the ORIGINAL kind
		// (audio_group) so an operator viewing history sees the rotating-
		// route configuration, but the dialplan response is rewritten to
		// kind='audio' + concrete target so the Playback dialplan branch
		// still works without knowing about groups.
		if reservedRouteKind == "audio_group" {
			if reservedAudioGroupID == nil {
				h.Log.Warn("audio_group reservation missing group id", "did", e164)
				return deny("reservation_misconfigured")
			}
			fname, err := h.pickAudioGroupMember(ctx, *reservedAudioGroupID)
			if err != nil {
				h.Log.Error("audio_group pick failed", "err", err, "did", e164, "group_id", *reservedAudioGroupID)
				return deny("reservation_misconfigured")
			}
			// route_target becomes the specific picked file — both CDR
			// and dialplan response use it. Only the kind diverges.
			reservedRouteTarget = "didstorage/" + fname
		}
		if reservedRouteTarget == "" {
			h.Log.Warn("reserved DID has no route target", "did", e164, "kind", reservedRouteKind)
			return deny("reservation_misconfigured")
		}
		// CDR + livecalls.Register both see the original (possibly
		// audio_group) kind so audit history shows what was configured.
		h.unbilledCDR(ctx, req, supplierID, didID, "reserved", reservedRouteKind, reservedRouteTarget)
		// dialKind is what we hand the dialplan — audio_group is
		// materialised to 'audio' here so the dialplan's existing 'audio'
		// Playback branch picks it up without any new dispatch.
		dialKind := reservedRouteKind
		if dialKind == "audio_group" {
			dialKind = "audio"
		}
		if err := livecalls.Register(ctx, h.Redis, livecalls.ActiveCall{
			CallID:          domain.SanitizeCallID(req.CallID),
			CallIDFull:      req.CallID,
			SupplierID:      supplierID,
			DIDID:           didID,
			E164:            e164,
			SrcIP:           req.SrcIP,
			SrcURI:          req.FromURI,
			RouteKind:       reservedRouteKind,
			RouteTarget:     reservedRouteTarget,
			Reserved:        true,
			AsteriskChannel: req.AsteriskChannel,
		}); err != nil {
			h.Log.Warn("livecalls.Register failed (reserved)", "err", err, "call_id", req.CallID)
		}
		h.Log.Info("authorize allow (reserved DID)",
			"call_id", req.CallID, "did", e164,
			"route_kind", reservedRouteKind, "route_target", reservedRouteTarget)
		// MaxSeconds picks a runaway ceiling appropriate for the branch:
		//
		//   audio / audio_group → admin-configurable via the
		//     `sip.reserved_audio_max_seconds` setting (see migration 0020).
		//     Default 300s (5 min) fits any realistic announcement, IVR
		//     intro, or hold-music loop. Prevents a bogus long or looping
		//     clip (or a Playback that never returns because the file is
		//     missing and Asterisk sat retrying) from leaving the channel
		//     open indefinitely and inflating channel counts / CDR
		//     durations downstream. Admin can lower it (aggressive runaway
		//     guard for tight integrators) or raise it (long recordings on
		//     reserved DIDs) from /settings without a redeploy.
		//
		//   sip_uri / ip / sip_account → 4h. Reserved DIDs are commonly
		//     used for internal forward destinations that legitimately host
		//     long-running calls; the 4h ceiling matches the L() runaway
		//     guard on billed customer calls.
		//
		// The dialplan enforces this via Set(TIMEOUT(absolute)=…) on the
		// audio branch and via Dial(..., ${AUTH_MAX_SECONDS}, L(…)) on the
		// forward branches.
		maxSec := 4 * 60 * 60
		if dialKind == "audio" {
			maxSec = settings.GetInt("sip.reserved_audio_max_seconds", 300)
		}
		return AuthorizeResponse{
			Decision:      "allow",
			ReservationID: req.CallID,
			MaxSeconds:    maxSec,
			RouteKind:     dialKind,
			RouteTarget:   reservedRouteTarget,
		}
	}

	// DID is in our system but not currently assigned to an active order
	// (most likely 'available' between rentals). Record a zero-charge CDR
	// for auditability AND mirror to denied_calls for the abuse view.
	if didStatus != "assigned" {
		h.unbilledCDR(ctx, req, supplierID, didID, "did_not_assigned", "", "")
		h.deniedCall(ctx, req, "did_not_assigned")
		return deny("did_not_assigned")
	}

	// 2. DID is assigned — load the order plus user and rate card.
	// We include 'kyc_pending' here so we can return a distinct deny
	// reason for it instead of the misleading 'did_not_assigned'. ORDER
	// BY puts 'active' first, then 'quarantined', then 'kyc_pending' —
	// so if an order somehow has multiple rows for one DID (data drift),
	// we pick the most-usable one.
	var orderAudioGroupID *int64
	err = h.DB.QueryRow(ctx, `
		SELECT o.id, o.user_id, o.channel_count, o.route_kind, o.route_target, o.pop_id,
		       o.rate_card_id, o.status::text,
		       u.balance_cents, u.global_channel_cap, u.status,
		       r.per_minute_cents,
		       o.audio_group_id
		  FROM orders o
		  JOIN users u       ON u.id = o.user_id
		  JOIN rate_cards r  ON r.id = o.rate_card_id
		 WHERE o.did_id = $1 AND o.status IN ('active','quarantined','kyc_pending')
		 ORDER BY CASE o.status
		            WHEN 'active'      THEN 1
		            WHEN 'quarantined' THEN 2
		            WHEN 'kyc_pending' THEN 3
		            ELSE 4
		          END
		 LIMIT 1
	`, didID).Scan(
		&orderID, &userID, &channelCount, &routeKind, &routeTarget, &popID,
		&rateCardID, &orderStatus,
		&userBalance, &userChannelCap, &userStatus,
		&ratePerMin,
		&orderAudioGroupID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// DID is marked assigned but no order found — data drift; treat as
		// unknown for the caller, log for ops, but still produce an audit
		// CDR for the call.
		h.Log.Warn("assigned DID with no order", "did", e164, "did_id", didID)
		h.unbilledCDR(ctx, req, supplierID, didID, "did_not_assigned", "", "")
		h.deniedCall(ctx, req, "did_not_assigned")
		return deny("did_not_assigned")
	}
	if err != nil {
		h.Log.Error("authorize order lookup failed", "err", err, "call_id", req.CallID)
		return deny("lookup_error")
	}

	// 2a. KYC-pending order → its own deny reason so diagnostics distinguish
	// "no order at all" from "order exists but waiting on KYC approval".
	// Upstream maps cause 31 → SIP 480, which most carriers play as a brief
	// "unavailable" prompt rather than the long "could not connect".
	// 2a. KYC-pending: deny with the unified busy-tone cause. denialCDR
	// preserves the precise reason for /cdrs diagnostics.
	if orderStatus == "kyc_pending" {
		h.denialCDR(ctx, req, orderID, userID, supplierID, didID, ratePerMin, "kyc_pending", routeKind, routeTarget)
		return AuthorizeResponse{
			Decision:    "deny",
			Reason:      "kyc_pending",
			HangupCause: causeForReason("kyc_pending"),
		}
	}

	// 2b. Quarantined order: same treatment. Reason on the AuthorizeResponse
	// (and the CDR) is the source-of-truth for what actually happened; the
	// wire-level SIP code stays uniform.
	if orderStatus == "quarantined" {
		h.denialCDR(ctx, req, orderID, userID, supplierID, didID, ratePerMin, "quarantined", routeKind, routeTarget)
		return AuthorizeResponse{
			Decision:    "deny",
			Reason:      "quarantined",
			HangupCause: causeForReason("quarantined"),
		}
	}

	// 3. User must be active. If suspended/blocked → write a customer CDR.
	if userStatus != "active" {
		h.denialCDR(ctx, req, orderID, userID, supplierID, didID, ratePerMin, "user_blocked", routeKind, routeTarget)
		return deny("user_blocked")
	}

	// 4. Balance gate.
	maxSeconds := domain.MaxSecondsForBalance(userBalance, ratePerMin)
	if maxSeconds < h.MinSecondsToAuthorize {
		h.denialCDR(ctx, req, orderID, userID, supplierID, didID, ratePerMin, "insufficient_balance", routeKind, routeTarget)
		return deny("insufficient_balance")
	}

	// 5. Reserve a channel in Redis (atomic check + add for both caps).
	// userChannelCap is -1 for uncapped (matches the Lua script's check).
	res, err := reserveChannel(ctx, h.Redis, userID, didID, req.CallID, userChannelCap, channelCount)
	if err != nil {
		h.Log.Error("authorize redis reserve failed", "err", err)
		return deny("reservation_error")
	}
	switch res {
	case 1:
		// reserved
	case -1, -2:
		h.denialCDR(ctx, req, orderID, userID, supplierID, didID, ratePerMin, "insufficient_channels", routeKind, routeTarget)
		if res == -1 {
			return deny("user_channel_cap_hit")
		}
		return deny("did_channel_cap_hit")
	default:
		return deny("reservation_unknown")
	}

	// audio_group on an order: resolve to one clip at INVITE time via
	// random-no-repeat (same logic as reservations). The DB-level
	// route_target is NULL for audio_group orders, so we materialise it
	// here. livecalls + the eventual /sipctl/cdr both see the resolved
	// target, giving the operator audit visibility into exactly which
	// clip played per call. The kind stays 'audio_group' for that audit
	// trail, but we hand the dialplan kind='audio' so its existing
	// Playback branch picks it up unchanged.
	dialKind := routeKind
	if routeKind == "audio_group" {
		if orderAudioGroupID == nil {
			h.Log.Warn("audio_group order missing group id", "order_id", orderID)
			return deny("reservation_misconfigured")
		}
		fname, err := h.pickAudioGroupMember(ctx, *orderAudioGroupID)
		if err != nil {
			h.Log.Error("audio_group pick failed (order)", "err", err, "order_id", orderID, "group_id", *orderAudioGroupID)
			return deny("reservation_misconfigured")
		}
		routeTarget = "didstorage/" + fname
		dialKind = "audio"
	}

	if err := livecalls.Register(ctx, h.Redis, livecalls.ActiveCall{
		CallID:          domain.SanitizeCallID(req.CallID),
		CallIDFull:      req.CallID,
		UserID:          userID,
		OrderID:         orderID,
		SupplierID:      supplierID,
		DIDID:           didID,
		E164:            e164,
		SrcIP:           req.SrcIP,
		SrcURI:          req.FromURI,
		RouteKind:       routeKind,
		RouteTarget:     routeTarget,
		RatePerMinCents: ratePerMin,
		AsteriskChannel: req.AsteriskChannel,
	}); err != nil {
		h.Log.Warn("livecalls.Register failed", "err", err, "call_id", req.CallID)
	}

	h.Log.Info("authorize allow",
		"call_id", req.CallID,
		"did", e164,
		"order_id", orderID,
		"user_id", userID,
		"max_seconds", maxSeconds,
		"rate_per_min_cents", ratePerMin,
		"route_kind", routeKind,
	)

	popOut := int64(0)
	if popID != nil {
		popOut = *popID
	}
	return AuthorizeResponse{
		Decision:        "allow",
		ReservationID:   req.CallID,
		MaxSeconds:      maxSeconds,
		RouteKind:       dialKind,
		RouteTarget:     routeTarget,
		POPID:           popOut,
		RateCentsPerMin: ratePerMin,
	}
}

func (h *Handler) cdr(w http.ResponseWriter, r *http.Request) {
	var req CDRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Always release the reservation, even if the rest fails.
	defer func() {
		if err := releaseChannelByCallID(r.Context(), h.Redis, req.CallID); err != nil {
			h.Log.Warn("cdr release failed", "err", err, "call_id", req.CallID)
		}
		// Deregister from the live-calls index so /live drops the row.
		// Best-effort; we never want a Redis hiccup to fail the CDR insert.
		if err := livecalls.Deregister(r.Context(), h.Redis, domain.SanitizeCallID(req.CallID)); err != nil {
			h.Log.Warn("livecalls.Deregister failed", "err", err, "call_id", req.CallID)
		}
	}()

	sanitizedCallID := domain.SanitizeCallID(req.CallID)
	var (
		orderID, userID, supplierID, didID int64
		popID                              *int64
		ratePerMin                         float64
		supPerMin                          float64
		billMin, billInc                   int
		supBillMin, supBillInc             int
		routedKind, routedTarget           string
	)
	err := h.DB.QueryRow(r.Context(), `
		SELECT o.id, o.user_id, d.supplier_id, d.id, o.pop_id,
		       rc.per_minute_cents,
		       rc.bill_min_seconds,
		       rc.bill_increment_seconds,
		       rc.supplier_per_minute_cents,
		       rc.supplier_bill_min_seconds,
		       rc.supplier_bill_increment_seconds,
		       o.route_kind::text, o.route_target
		  FROM orders o
		  JOIN dids d         ON d.id = o.did_id
		  JOIN rate_cards rc  ON rc.id = o.rate_card_id
		 WHERE o.id = (
		   SELECT order_id FROM cdrs WHERE call_id = $1 LIMIT 1
		 ) OR o.id IN (
		   SELECT o2.id FROM orders o2
		    JOIN dids d2 ON d2.id = o2.did_id
		   WHERE o2.status = 'active' AND d2.e164 = $2
		   LIMIT 1
		 )
		 LIMIT 1
	`, sanitizedCallID, parseE164(req.DstURI)).Scan(&orderID, &userID, &supplierID, &didID, &popID,
		&ratePerMin, &billMin, &billInc,
		&supPerMin, &supBillMin, &supBillInc,
		&routedKind, &routedTarget)
	if err != nil {
		// No matching order — but a CDR shell may already exist (reserved
		// DID, did_not_assigned, etc., written by /authorize via unbilledCDR
		// or denialCDR). Fill in the call's actual timing/duration on that
		// row; no charge / ledger to post.
		if errors.Is(err, pgx.ErrNoRows) {
			h.fillUnbilledCDR(r.Context(), sanitizedCallID, req)
			writeJSON(w, http.StatusOK, CDRResponse{Status: "ok"})
			return
		}
		h.Log.Error("cdr lookup failed", "err", err, "call_id", req.CallID, "dst_uri", req.DstURI)
		writeJSON(w, http.StatusOK, CDRResponse{Status: "no_match"})
		return
	}

	// Customer-side charge — uses the rate card's per-min + min/inc at THIS
	// call's instant. We persist the bill_min/inc on the cdrs row too so any
	// later rate-card edit can't rewrite history. billedSeconds is the
	// post-round duration the customer was actually charged for; useful for
	// the operator-facing CDR list ("Billed 90s @ 60/30" vs raw 75s).
	billedSec, chargedMin, chargeCents := domain.ChargeForCall(req.Billsec, billMin, billInc, ratePerMin)
	_ = billedSec
	// Supplier-side cost snapshot — same idea on the other side.
	supChargeCents := domain.SupplierChargeForCall(req.Billsec, supBillMin, supBillInc, supPerMin)

	startedAt := time.Unix(req.StartedAt, 0).UTC()
	endedAt := time.Unix(req.EndedAt, 0).UTC()
	var answeredAt *time.Time
	if req.AnsweredAt > 0 {
		t := time.Unix(req.AnsweredAt, 0).UTC()
		answeredAt = &t
	}

	// Pop any pending admin-action stash (Hangup/Warn/Redirect from /live).
	// If present, the cdrs row gets stamped with admin_action* columns so
	// the /cdrs list visibly distinguishes it from a normal caller hangup.
	// Best-effort: a Redis hiccup here just loses the admin-action label;
	// the CDR itself still records.
	var adminAction *string
	var adminActionBy *int64
	var adminActionReason *string
	if pa, perr := livecalls.PopPendingAction(r.Context(), h.Redis, sanitizedCallID); perr == nil && pa != nil {
		adminAction = &pa.Action
		if pa.AdminID > 0 {
			adminActionBy = &pa.AdminID
		}
		if pa.Reason != "" {
			adminActionReason = &pa.Reason
		}
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.Log.Error("cdr begin failed", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	_, err = tx.Exec(r.Context(), `
		INSERT INTO cdrs (call_id, order_id, user_id, supplier_id, did_id, pop_id,
		                  routed_kind, routed_target,
		                  started_at, answered_at, ended_at, billsec, charged_minutes,
		                  rate_cents_per_min, charge_cents,
		                  bill_min_seconds, bill_increment_seconds,
		                  supplier_charge_cents, supplier_bill_min_seconds, supplier_bill_increment_seconds,
		                  hangup_cause, src_uri, dst_uri,
		                  admin_action, admin_action_by, admin_action_reason)
		VALUES ($1,$2,$3,$4,$5,$6, $7::route_kind,$8,
		        $9,$10,$11,$12,$13, $14,$15, $16,$17, $18,$19,$20, $21,$22,$23,
		        $24::user_block_action, $25, $26)
		ON CONFLICT (call_id) DO UPDATE SET
		    admin_action        = COALESCE(EXCLUDED.admin_action,        cdrs.admin_action),
		    admin_action_by     = COALESCE(EXCLUDED.admin_action_by,     cdrs.admin_action_by),
		    admin_action_reason = COALESCE(EXCLUDED.admin_action_reason, cdrs.admin_action_reason)
	`, sanitizedCallID, orderID, userID, supplierID, didID, popID,
		routedKind, routedTarget,
		startedAt, answeredAt, endedAt, req.Billsec, chargedMin,
		ratePerMin, chargeCents,
		billMin, billInc,
		supChargeCents, supBillMin, supBillInc,
		req.HangupCause, req.SrcURI, req.DstURI,
		adminAction, adminActionBy, adminActionReason)
	if err != nil {
		h.Log.Error("cdr insert failed", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	if chargeCents > 0 {
		var balanceAfter int64
		err = tx.QueryRow(r.Context(), `
			UPDATE users SET balance_cents = balance_cents - $1
			 WHERE id = $2
			 RETURNING balance_cents
		`, chargeCents, userID).Scan(&balanceAfter)
		if err != nil {
			h.Log.Error("cdr balance update failed", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		_, err = tx.Exec(r.Context(), `
			INSERT INTO balance_ledger (user_id, delta_cents, kind, ref_table, ref_id, balance_after)
			VALUES ($1, $2, 'call_charge', 'cdrs', (SELECT id FROM cdrs WHERE call_id = $3), $4)
		`, userID, -int64(chargeCents), sanitizedCallID, balanceAfter)
		if err != nil {
			h.Log.Error("cdr ledger insert failed", "err", err)
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.Log.Error("cdr commit failed", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	h.Log.Info("cdr charged",
		"call_id", req.CallID,
		"billsec", req.Billsec,
		"charged_minutes", chargedMin,
		"charge_cents", chargeCents,
		"order_id", orderID,
		"user_id", userID,
	)
	// Kick off the background trace precompute so the SIP trace page is
	// instant the first time an admin opens it after the call ends.
	h.precomputeTrace(sanitizedCallID)
	writeJSON(w, http.StatusOK, CDRResponse{Status: "ok", ChargeCents: chargeCents})
}

const reserveLua = `
local user_cap = tonumber(ARGV[2])
local did_cap  = tonumber(ARGV[3])
local user_active = redis.call('SCARD', KEYS[1])
local did_active  = redis.call('SCARD', KEYS[2])
if user_cap >= 0 and user_active >= user_cap then return -1 end
if did_active >= did_cap then return -2 end
redis.call('SADD', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], 14400)
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('EXPIRE', KEYS[2], 14400)
return 1
`

func reserveChannel(ctx context.Context, rdb *redis.Client, userID, didID int64, callID string, userCap, didCap int) (int64, error) {
	keys := []string{
		fmt.Sprintf("act:user:%d", userID),
		fmt.Sprintf("act:did:%d", didID),
	}
	res, err := rdb.Eval(ctx, reserveLua, keys, callID, userCap, didCap).Result()
	if err != nil {
		return 0, err
	}
	n, _ := res.(int64)
	return n, nil
}

func releaseChannelByCallID(ctx context.Context, rdb *redis.Client, callID string) error {
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "act:*", 200).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			pipe := rdb.Pipeline()
			for _, k := range keys {
				pipe.SRem(ctx, k, callID)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return nil
}

func parseE164(uri string) string {
	uri = strings.TrimSpace(uri)
	uri = strings.TrimPrefix(uri, "<")
	uri = strings.TrimSuffix(uri, ">")
	if i := strings.Index(uri, ":"); i >= 0 {
		uri = uri[i+1:]
	}
	if i := strings.Index(uri, "@"); i >= 0 {
		uri = uri[:i]
	}
	uri = strings.TrimPrefix(uri, "+")
	var b strings.Builder
	for _, r := range uri {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
