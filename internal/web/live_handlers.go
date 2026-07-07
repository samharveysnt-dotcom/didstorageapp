package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"didstorage/internal/audiogroup"
	"didstorage/internal/auth"
	"didstorage/internal/domain"
	"didstorage/internal/livecalls"
)

// liveCalls renders the Live · Active calls page — a table of every in-flight
// call the platform has authorized but not yet seen a /sipctl/cdr for. The
// initial page render is server-rendered HTML; once loaded, the page opens
// a Server-Sent Events stream at /live/stream to receive snapshots without
// the meta-refresh flicker.
//
// Source of truth is Redis (`live:active` ZSET + `live:meta:*` keys),
// populated by /authorize allow paths and cleared by /cdr.
func (h *Handler) liveCalls(w http.ResponseWriter, r *http.Request) {
	rows, err := h.fetchLiveRows(r.Context())
	if err != nil {
		h.Log.Error("livecalls.List", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	ok, em := h.popFlashes(r)
	h.render(w, "live", map[string]any{
		"Title":    "Live calls",
		"Section":  "live",
		"FlashOK":  ok,
		"FlashErr": em,
		"Calls":    rows,
		"Total":    len(rows),
	})
}

// liveStream is the SSE endpoint the /live page subscribes to on load. We
// push a JSON snapshot every second; clients render the table from each
// snapshot. SSE was chosen over WebSocket because we only need server→client
// pushes here (the hangup action goes through a normal POST), and SSE is
// stdlib-only, plays nicely with HTTP/1.1 proxies, and gets free
// auto-reconnect from the EventSource browser API.
func (h *Handler) liveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering if any

	send := func(event string, payload any) error {
		blob, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", blob)
		flusher.Flush()
		return nil
	}

	// Initial snapshot so the page populates immediately.
	if rows, err := h.fetchLiveRows(r.Context()); err == nil {
		_ = send("snapshot", rows)
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	// Heartbeat comments every 15s so a load balancer's idle timeout doesn't
	// close the connection during quiet periods.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			rows, err := h.fetchLiveRows(r.Context())
			if err != nil {
				h.Log.Warn("livecalls stream fetch", "err", err)
				continue
			}
			if err := send("snapshot", rows); err != nil {
				return
			}
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// fetchLiveRows enriches every Redis-tracked call with DB-side display info
// (customer label, DID country/type, reseller). Shared between the initial
// page render and the SSE snapshot loop so both views always show identical
// data.
type liveRow struct {
	CallID       string  `json:"call_id"`
	StartedAtUTC string  `json:"started_at_utc"`
	StartedAtUnix int64  `json:"started_at_unix"`
	E164         string  `json:"e164"`
	// DIDID drives the e164 → /dids/{id}/cdrs link in the live table.
	// Without this field the template's {{if .DIDID}} branch errors out
	// and the row halts mid-render, leaving a "render error" sliver.
	DIDID        int64   `json:"did_id,omitempty"`
	Country      string  `json:"country,omitempty"`
	DIDType      string  `json:"did_type,omitempty"`
	UserID       int64   `json:"user_id,omitempty"`
	UserRef      string  `json:"user_ref,omitempty"`
	OrderID      int64   `json:"order_id,omitempty"`
	ResellerID   int64   `json:"reseller_id,omitempty"`
	Reseller     string  `json:"reseller,omitempty"`
	SrcURI       string  `json:"src_uri,omitempty"`
	SrcURIBare   string  `json:"src_uri_bare,omitempty"`
	SrcIP        string  `json:"src_ip"`
	RouteKind    string  `json:"route_kind"`
	RouteTarget  string  `json:"route_target"`
	Reserved     bool    `json:"reserved"`
	// LastAdminAction is set when an admin acted on this call mid-flight
	// (today: only `redirect`, since warn + hangup end the call and the
	// row drops out of the index instead). The /live UI surfaces this as
	// a small pill so the admin can tell the row's been acted on.
	LastAdminAction string `json:"last_admin_action,omitempty"`

	// State is a short ops-friendly call-state label derived from the
	// live Asterisk channel: "answered" (Up), "ringing" (Ring/Dialing/
	// Pre-Ring), "connecting" (Down/Reserved/Off-Hook). Empty when the
	// channel can't be found (pre-deploy call without a captured channel
	// name, or post-hangup race). Drives the Status pill in /live so an
	// admin can distinguish a call that's still ringing from one that's
	// actually got media flowing.
	State string `json:"state,omitempty"`
}

func (h *Handler) fetchLiveRows(ctx context.Context) ([]liveRow, error) {
	calls, err := livecalls.List(ctx, h.Redis)
	if err != nil {
		return nil, err
	}
	// Snapshot every active Asterisk channel's state once per request; per-call
	// lookups below are a map read. Cheap even at hundreds of channels — one
	// `asterisk -rx` call versus N per call. Errors degrade gracefully: the
	// State field stays empty and the GUI shows "—".
	states := channelStates(ctx)
	rows := make([]liveRow, 0, len(calls))
	for _, c := range calls {
		r0 := liveRow{
			CallID:          c.CallID,
			StartedAtUTC:    time.Unix(c.StartedAt, 0).UTC().Format("15:04:05"),
			StartedAtUnix:   c.StartedAt,
			E164:            c.E164,
			DIDID:           c.DIDID,
			SrcURI:          c.SrcURI,
			SrcURIBare:      domain.CleanCallerURI(c.SrcURI),
			SrcIP:           c.SrcIP,
			RouteKind:       c.RouteKind,
			RouteTarget:     c.RouteTarget,
			UserID:          c.UserID,
			OrderID:         c.OrderID,
			Reserved:        c.Reserved,
			LastAdminAction: c.LastAdminAction,
			State:           callStateLabel(states[c.AsteriskChannel]),
		}
		if c.DIDID > 0 {
			_ = h.DB.QueryRow(ctx,
				`SELECT country_iso, did_type::text FROM dids WHERE id=$1`,
				c.DIDID).Scan(&r0.Country, &r0.DIDType)
		}
		if c.UserID > 0 {
			_ = h.DB.QueryRow(ctx, `
				SELECT COALESCE(u.external_id, u.label, u.contact_email, ''),
				       COALESCE(re.name, ''),
				       COALESCE(u.reseller_id, 0)
				  FROM users u LEFT JOIN resellers re ON re.id = u.reseller_id
				 WHERE u.id=$1`, c.UserID).Scan(&r0.UserRef, &r0.Reseller, &r0.ResellerID)
		}
		rows = append(rows, r0)
	}
	return rows, nil
}

// liveHangup forcibly tears down an in-flight call.
//
// Asterisk's `pjsip show channels` doesn't expose the SIP Call-ID, so we
// match by the dialed-DID column (Exten) — that uniquely identifies the
// inbound leg in 99% of cases (the rare exception being two simultaneous
// calls to the same DID, capped by the order's channel_count anyway).
// Hanging up the inbound leg cascades the BYE through the bridge and ends
// the outbound leg too.
//
// Audit-logged at INFO with the admin id + call_id + reason + channel name.
func (h *Handler) liveHangup(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "call_id")
	if callID == "" {
		h.flashErr(r, "no call_id")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		reason = "(no reason provided)"
	}
	adminID := auth.AdminIDFromSession(h.Session, r)

	ac, err := livecalls.Get(r.Context(), h.Redis, callID)
	if err != nil {
		h.Log.Error("livecalls.Get", "err", err, "call_id", callID)
		h.flashErr(r, "redis: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if ac == nil {
		h.flashErr(r, "call no longer active (already ended?)")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	chanName, err := resolveChannel(r.Context(), ac)
	if err != nil {
		h.Log.Warn("admin hangup — channel lookup failed",
			"err", err, "admin_id", adminID, "call_id", callID, "reason", reason)
		h.flashErr(r, "channel lookup: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if chanName == "" {
		h.Log.Info("admin hangup — channel not found in Asterisk (already gone)",
			"admin_id", adminID, "call_id", callID, "e164", ac.E164, "reason", reason)
		h.writeComplianceEvent(r.Context(), ac.UserID, ac.OrderID, adminID,
			"live_hangup", reason, fmt.Sprintf("call_id=%s outcome=already_gone", callID))
		// Mark a pending action anyway — if the CDR fires after this point
		// (Asterisk's BYE might still be in flight), the CDR row will carry
		// the admin-hangup label.
		_ = livecalls.RecordPendingAction(r.Context(), h.Redis, callID, "live_hangup", adminID, reason)
		_ = livecalls.Deregister(r.Context(), h.Redis, callID)
		h.flashOK(r, "Call was already ending. Removed from live list.")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	if err := asteriskHangup(r.Context(), chanName); err != nil {
		h.Log.Error("admin hangup — failed", "err", err,
			"admin_id", adminID, "call_id", callID, "channel", chanName, "reason", reason)
		h.flashErr(r, "hangup: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	h.Log.Info("admin hangup — success",
		"admin_id", adminID,
		"call_id", callID,
		"channel", chanName,
		"e164", ac.E164,
		"user_id", ac.UserID,
		"reason", reason,
	)
	h.writeComplianceEvent(r.Context(), ac.UserID, ac.OrderID, adminID,
		"live_hangup", reason, fmt.Sprintf("call_id=%s channel=%s", callID, chanName))
	// Stash a pending-action marker so /sipctl/cdr stamps admin_action on
	// the cdrs row when the hangup_handler AGI fires shortly.
	_ = livecalls.RecordPendingAction(r.Context(), h.Redis, callID, "live_hangup", adminID, reason)
	_ = livecalls.Deregister(r.Context(), h.Redis, callID)
	h.flashOK(r, "Call hung up.")
	http.Redirect(w, r, "/live", http.StatusFound)
}

// liveForceCleanup evicts every live-call entry whose AsteriskChannel
// either is empty OR is no longer present in Asterisk's current channel
// list. Used to clear "ghost" rows left behind when the matching
// /sipctl/cdr never fired — most commonly from a manual /sipctl/authorize
// probe during diagnostics, or a real call whose channel died abruptly
// without a SIP BYE making it to the dialplan's hangup handler.
//
// Safe to call any time: rows whose channel IS live in Asterisk are left
// alone, so this can't accidentally drop a real in-flight call from the
// admin's view. For each evicted entry we also SREM any act:user:* /
// act:did:* channel-reservation membership so the count gauges and
// concurrency caps don't stay over-counted.
//
// Audit-logged at INFO with the admin id and the evicted call_ids so a
// later "where did call X go" question can be answered from journald.
func (h *Handler) liveForceCleanup(w http.ResponseWriter, r *http.Request) {
	adminID := auth.AdminIDFromSession(h.Session, r)

	// 1. Snapshot the current Asterisk channel set — names only, state
	//    doesn't matter for the keep/evict decision.
	chans := channelStates(r.Context())

	// 2. Snapshot live-call entries from Redis.
	calls, err := livecalls.List(r.Context(), h.Redis)
	if err != nil {
		h.Log.Error("live force-cleanup list", "err", err)
		h.flashErr(r, "couldn't list live calls: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	// 3. Decide + evict. Capture evicted ids for the flash + audit log.
	var evicted []string
	var kept int
	for _, ac := range calls {
		// Channel name empty → call never made it to a real PJSIP channel
		// (synthetic /sipctl/authorize probe). Evict.
		// Channel name present but not in current channel set → channel
		// is already gone, /sipctl/cdr just never landed. Evict.
		if ac.AsteriskChannel == "" {
			evicted = append(evicted, ac.CallID)
			continue
		}
		if _, alive := chans[ac.AsteriskChannel]; !alive {
			evicted = append(evicted, ac.CallID)
			continue
		}
		kept++
	}

	if len(evicted) == 0 {
		h.flashOK(r, fmt.Sprintf("Nothing to clean — all %d live entr%s match a real Asterisk channel.",
			kept, func() string { if kept == 1 { return "y" }; return "ies" }()))
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	// 4. Actually remove. Each Deregister is a pipelined ZREM+DEL; we also
	//    drop channel-reservation membership across every act:* SET so the
	//    user/DID concurrency counters don't stay over-counted.
	for _, cid := range evicted {
		_ = livecalls.Deregister(r.Context(), h.Redis, cid)
	}
	if err := releaseChannelReservations(r.Context(), h.Redis, evicted); err != nil {
		h.Log.Warn("live force-cleanup channel-reservation sweep", "err", err)
	}

	h.Log.Info("live force-cleanup",
		"admin_id", adminID,
		"evicted_count", len(evicted),
		"kept_count", kept,
		"evicted_call_ids", strings.Join(evicted, ","))
	h.flashOK(r, fmt.Sprintf("Removed %d ghost entr%s (kept %d live).",
		len(evicted),
		func() string { if len(evicted) == 1 { return "y" }; return "ies" }(),
		kept))
	http.Redirect(w, r, "/live", http.StatusFound)
}

// releaseChannelReservations SCANs every act:* SET in Redis and SREMs each
// provided call_id from it. Mirrors sipctl.releaseChannelByCallID, which
// is package-private — this is the same logic inlined so the web package
// doesn't have to import sipctl. Bounded SCAN cursor + pipelined SREMs,
// so worst-case it's one round trip per Redis key page (200 keys/page).
func releaseChannelReservations(ctx context.Context, rdb *redis.Client, callIDs []string) error {
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "act:*", 200).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			pipe := rdb.Pipeline()
			for _, k := range keys {
				for _, cid := range callIDs {
					pipe.SRem(ctx, k, cid)
				}
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}

// channelStates queries Asterisk for every active channel's state, keyed by
// channel name (e.g. "PJSIP/supplier-trunk-00000063"). Returns nil on any
// failure so callers can degrade to "unknown state" without bailing the
// whole /live render.
//
// Uses `core show channels concise`: pipe-separated, one channel per line,
// stable across Asterisk versions. Field 0 is the channel name, field 4 is
// the state ("Up", "Ringing", "Dialing", ...). We don't care about the
// other fields here. Output ends with a "N active channel" footer line we
// skip via the field-count guard.
func channelStates(ctx context.Context) map[string]string {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "asterisk", "-rx", "core show channels concise").CombinedOutput()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, "!")
		if len(fields) < 5 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		state := strings.TrimSpace(fields[4])
		if name == "" || state == "" {
			continue
		}
		m[name] = state
	}
	return m
}

// callStateLabel collapses Asterisk's raw channel-state vocabulary to the
// three buckets the /live page colour-codes. Everything that means "media
// is flowing" → answered; everything pre-answer → ringing / connecting;
// unknown / empty → "" (which the template renders as a muted em-dash).
func callStateLabel(raw string) string {
	switch raw {
	case "Up":
		return "answered"
	case "Ring", "Ringing", "Dialing", "Dialing Offhook", "Pre-Ring":
		return "ringing"
	case "Down", "Reserved", "Off-Hook":
		return "connecting"
	case "":
		return ""
	}
	return strings.ToLower(raw)
}

// resolveChannel returns the Asterisk channel name for an in-flight call.
//
// Preferred path: the channel name we captured at /sipctl/authorize via
// the AGI's ${CHANNEL} arg and stored in livecalls metadata. This is
// reliable regardless of dialplan transfers — warn/redirect change the
// channel's current Exten (into [admin-actions]) but not its name.
//
// Fallback: legacy live calls registered before the AsteriskChannel
// field existed have ac.AsteriskChannel == "". We fall back to the
// `pjsip show channels` Exten-grep so a fresh-deploy mid-call doesn't
// strand existing live rows. The fallback is the same dialed-DID match
// the live actions used historically — fragile when two calls share a
// DID or when one of those has been redirected, but it's only ever hit
// for pre-deploy in-flight calls.
func resolveChannel(ctx context.Context, ac *livecalls.ActiveCall) (string, error) {
	if ac == nil {
		return "", errors.New("nil ActiveCall")
	}
	if ac.AsteriskChannel != "" {
		return ac.AsteriskChannel, nil
	}
	return findInboundLegChannel(ctx, ac.E164, ac.SrcURI)
}

// pjsipChannelRE captures the Asterisk channel name from a "Channel:" line.
// pjsipExtenRE captures the dialed-Exten + CLCID from the follow-up line.
var (
	pjsipChannelRE = regexp.MustCompile(`Channel:\s+(\S+)`)
	pjsipExtenRE   = regexp.MustCompile(`Exten:\s*(\S*)\s+CLCID:\s*(.*)$`)
	clcidNumberRE  = regexp.MustCompile(`<([^>]+)>`)
)

// findInboundLegChannel parses `asterisk -rx "pjsip show channels"` and
// returns the inbound-leg channel name whose dialed Exten matches our DID.
// Channels are listed as paired lines:
//
//	  Channel: PJSIP/globetelecom-0000003b/Dial   Up   00:03:05
//	      Exten: 442038968001    CLCID: "" <442038968001>
//
// We strip the trailing `/Dial` / `/AppDial` application-suffix from the
// channel name — `channel request hangup` wants the bare PJSIP/name.
// CLCID is consulted as a tiebreaker when multiple channels share the same
// Exten (rare; would require simultaneous calls to the same DID).
func findInboundLegChannel(ctx context.Context, e164, srcURI string) (string, error) {
	if e164 == "" {
		return "", errors.New("no E.164 on the call metadata — can't match a channel")
	}
	out, err := exec.CommandContext(ctx, "asterisk", "-rx", "pjsip show channels").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pjsip show channels: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	bareCaller := domain.CleanCallerURI(srcURI) // e.g. "447956816884"

	type candidate struct {
		chanName string
		clcid    string
	}
	var matches []candidate

	lines := strings.Split(string(out), "\n")
	for i := 0; i < len(lines); i++ {
		chMatch := pjsipChannelRE.FindStringSubmatch(lines[i])
		if len(chMatch) < 2 {
			continue
		}
		chanName := stripChannelSuffix(chMatch[1])
		// Look ahead a couple lines for the Exten/CLCID pair.
		var extenLine string
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if strings.Contains(lines[j], "Exten:") && strings.Contains(lines[j], "CLCID:") {
				extenLine = lines[j]
				break
			}
		}
		if extenLine == "" {
			continue
		}
		exMatch := pjsipExtenRE.FindStringSubmatch(extenLine)
		if len(exMatch) < 3 {
			continue
		}
		exten := strings.TrimSpace(exMatch[1])
		clcid := strings.TrimSpace(exMatch[2])
		// Bare-number out of `"" <447956816884>` if present.
		if mn := clcidNumberRE.FindStringSubmatch(clcid); len(mn) >= 2 {
			clcid = mn[1]
		}
		if exten == e164 {
			matches = append(matches, candidate{chanName, clcid})
		}
	}

	switch len(matches) {
	case 0:
		return "", nil
	case 1:
		return matches[0].chanName, nil
	}
	// More than one inbound leg to the same DID — try to disambiguate by
	// matching the call's source URI against the CLCID number.
	if bareCaller != "" {
		for _, c := range matches {
			if c.clcid != "" && (strings.Contains(c.clcid, bareCaller) || strings.Contains(bareCaller, c.clcid)) {
				return c.chanName, nil
			}
		}
	}
	// No tiebreak — return the first match. Admin will see the right call
	// drop; if the wrong one drops they can identify and hang up the other.
	return matches[0].chanName, nil
}

// stripChannelSuffix turns "PJSIP/globetelecom-0000003b/Dial" into
// "PJSIP/globetelecom-0000003b". The trailing "/Dial" or "/AppDial" is the
// running application's name (Asterisk PBX state), not part of the channel
// identifier the hangup command wants.
func stripChannelSuffix(s string) string {
	// PJSIP/name-id  → keep first two slashes, drop anything after.
	parts := strings.SplitN(s, "/", 3)
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return s
}

// asteriskHangup shells out the actual `channel request hangup` command.
func asteriskHangup(ctx context.Context, chanName string) error {
	if chanName == "" {
		return errors.New("empty channel name")
	}
	cmd := exec.CommandContext(ctx, "asterisk", "-rx", "channel request hangup "+chanName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("asterisk -rx: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// asteriskSetChanVar sets a single channel variable. Used by the redirect
// handler to pass arguments into the [admin-actions] dialplan context
// before transferring the channel there. value is single-token (matches
// Asterisk's CLI grammar — we wrap it in quotes only if it contains
// whitespace, which our generated values shouldn't).
func asteriskSetChanVar(ctx context.Context, chanName, name, value string) error {
	if chanName == "" || name == "" {
		return errors.New("empty channel name or variable name")
	}
	cmd := exec.CommandContext(ctx, "asterisk", "-rx",
		fmt.Sprintf("dialplan set chanvar %s %s %s", chanName, name, value))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("asterisk -rx setvar: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// asteriskTransfer moves the channel to <exten>@<context>:1 via the CLI.
// Same semantics as channel redirect — the channel drops out of its current
// bridge / application and resumes at the given dialplan slot.
func asteriskTransfer(ctx context.Context, chanName, exten, context string) error {
	if chanName == "" {
		return errors.New("empty channel name")
	}
	cmd := exec.CommandContext(ctx, "asterisk", "-rx",
		fmt.Sprintf("channel redirect %s %s,%s,1", chanName, context, exten))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("asterisk -rx redirect: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// liveRedirect retargets the in-flight call to a fresh destination. Drops
// the existing outbound leg, dials a new one, bridges to the caller.
//
// Builds the Asterisk Dial() string from route_kind + route_target with the
// same conventions /sipctl/authorize uses — so a redirect to e.g.
// "sip:904@example.com" reuses the same [outbound] PJSIP endpoint the
// initial route would have.
func (h *Handler) liveRedirect(w http.ResponseWriter, r *http.Request) {
	callID := chi.URLParam(r, "call_id")
	if callID == "" {
		h.flashErr(r, "no call_id")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	routeKind := strings.TrimSpace(r.PostForm.Get("route_kind"))
	routeTarget := strings.TrimSpace(r.PostForm.Get("route_target"))
	afIDStr := strings.TrimSpace(r.PostForm.Get("audio_file_id"))
	agIDStr := strings.TrimSpace(r.PostForm.Get("audio_group_id"))
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		reason = "(no reason provided)"
	}
	adminID := auth.AdminIDFromSession(h.Session, r)

	ac, err := livecalls.Get(r.Context(), h.Redis, callID)
	if err != nil {
		h.Log.Error("livecalls.Get", "err", err, "call_id", callID)
		h.flashErr(r, "redis: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if ac == nil {
		h.flashErr(r, "call no longer active")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	// Resolve the redirect target. SIP kinds turn into a PJSIP/... Dial()
	// string consumed by [admin-actions]:redirect; audio kinds turn into
	// an "audio basename" (didstorage/<filename>) consumed by [admin-
	// actions]:play-audio. audio_group is resolved to one specific clip
	// here using the same random-no-repeat helper /sipctl/authorize uses
	// for orders + reservations.
	var (
		dialString    string // for sip_uri / ip / sip_account
		audioBasename string // for audio / audio_group  (e.g. didstorage/af_xxx)
	)
	switch routeKind {
	case "audio":
		afID, _ := strconv.ParseInt(afIDStr, 10, 64)
		if afID <= 0 {
			h.flashErr(r, "pick an audio clip")
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		}
		var filename string
		if err := h.DB.QueryRow(r.Context(),
			`SELECT filename FROM audio_files WHERE id = $1`, afID).Scan(&filename); err != nil {
			h.flashErr(r, "audio clip lookup: "+err.Error())
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		}
		audioBasename = "didstorage/" + filename
		routeTarget = audioBasename
	case "audio_group":
		agID, _ := strconv.ParseInt(agIDStr, 10, 64)
		if agID <= 0 {
			h.flashErr(r, "pick an audio group")
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		}
		fname, err := audiogroup.PickMember(r.Context(), h.DB, h.Redis, agID)
		if err != nil {
			h.flashErr(r, "audio_group pick: "+err.Error())
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		}
		audioBasename = "didstorage/" + fname
		routeTarget = audioBasename
	default:
		s, err := buildRedirectDialString(routeKind, routeTarget, ac.E164)
		if err != nil {
			h.flashErr(r, "redirect target: "+err.Error())
			http.Redirect(w, r, "/live", http.StatusFound)
			return
		}
		dialString = s
	}

	chanName, err := resolveChannel(r.Context(), ac)
	if err != nil {
		h.Log.Warn("admin redirect — channel lookup failed",
			"err", err, "admin_id", adminID, "call_id", callID, "reason", reason)
		h.flashErr(r, "channel lookup: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if chanName == "" {
		h.Log.Info("admin redirect — channel not found in Asterisk (already gone)",
			"admin_id", adminID, "call_id", callID, "e164", ac.E164, "reason", reason)
		h.writeComplianceEvent(r.Context(), ac.UserID, ac.OrderID, adminID,
			"live_redirect", reason, fmt.Sprintf("call_id=%s route=%s:%s outcome=already_gone", callID, routeKind, routeTarget))
		_ = livecalls.RecordPendingAction(r.Context(), h.Redis, callID, "live_redirect", adminID,
			fmt.Sprintf("route=%s:%s · %s", routeKind, routeTarget, reason))
		_ = livecalls.Deregister(r.Context(), h.Redis, callID)
		h.flashOK(r, "Call was already ending. Removed from live list.")
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	// Dispatch on routeKind: SIP kinds set ADMIN_REDIRECT + transfer to
	// [admin-actions]:redirect (existing). Audio kinds set
	// ADMIN_AUDIO_TARGET + transfer to [admin-actions]:play-audio (new
	// extension shipped in asterisk/extensions.conf).
	var (
		chanvarName, chanvarVal string
		transferExten           string
	)
	switch routeKind {
	case "audio", "audio_group":
		chanvarName = "ADMIN_AUDIO_TARGET"
		chanvarVal = audioBasename
		transferExten = "play-audio"
	default:
		chanvarName = "ADMIN_REDIRECT"
		chanvarVal = dialString
		transferExten = "redirect"
	}
	if err := asteriskSetChanVar(r.Context(), chanName, chanvarName, chanvarVal); err != nil {
		h.Log.Error("admin redirect — setvar failed", "err", err,
			"admin_id", adminID, "call_id", callID, "channel", chanName, "var", chanvarName, "val", chanvarVal)
		h.flashErr(r, "set variable: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}
	if err := asteriskTransfer(r.Context(), chanName, transferExten, "admin-actions"); err != nil {
		h.Log.Error("admin redirect — transfer failed", "err", err,
			"admin_id", adminID, "call_id", callID, "channel", chanName, "exten", transferExten)
		h.flashErr(r, "transfer: "+err.Error())
		http.Redirect(w, r, "/live", http.StatusFound)
		return
	}

	h.Log.Info("admin redirect — success",
		"admin_id", adminID, "call_id", callID, "channel", chanName,
		"e164", ac.E164, "user_id", ac.UserID,
		"route_kind", routeKind, "route_target", routeTarget,
		"transfer_exten", transferExten, "reason", reason)
	h.writeComplianceEvent(r.Context(), ac.UserID, ac.OrderID, adminID,
		"live_redirect", reason, fmt.Sprintf("call_id=%s route=%s:%s", callID, routeKind, routeTarget))
	_ = livecalls.RecordPendingAction(r.Context(), h.Redis, callID, "live_redirect", adminID,
		fmt.Sprintf("route=%s:%s · %s", routeKind, routeTarget, reason))
	// Audio redirects end the call (Playback → Hangup) — there's no
	// new bridge to keep "live". Deregister now so the row clears. SIP
	// redirects update the meta and stay live for the new outbound leg.
	if routeKind == "audio" || routeKind == "audio_group" {
		_ = livecalls.Deregister(r.Context(), h.Redis, callID)
		h.flashOK(r, "Call redirected to audio playback. Will end after the clip.")
	} else {
		if err := livecalls.UpdateRoute(r.Context(), h.Redis, callID, routeKind, routeTarget, "redirect"); err != nil {
			h.Log.Warn("admin redirect — update live meta failed", "err", err, "call_id", callID)
		}
		h.flashOK(r, "Call redirected to "+routeTarget+". Still active on the live list.")
	}
	http.Redirect(w, r, "/live", http.StatusFound)
}

// writeComplianceEvent inserts a row into user_block_log capturing an admin
// action against a specific call/order. Best-effort: a Redis-side success
// is the source of truth for the action itself; if the audit write fails we
// log a warning but don't surface it as an error to the admin (the action
// has already happened on Asterisk's side, and re-trying would be confusing).
//
// userID/orderID may be 0 for reserved-DID short-circuit calls (no user/
// order attached) — we still log the event with NULLs for those columns.
// adminID 0 means session-less invocation (shouldn't normally happen but
// defensive).
func (h *Handler) writeComplianceEvent(ctx context.Context, userID, orderID, adminID int64, action, reason, details string) {
	if userID == 0 {
		// Reserved-DID calls have no user — we still want the action on the
		// journal but there's no FK target for user_block_log.user_id. Skip
		// silently; the structured-log line above is the audit record.
		return
	}
	var orderArg, adminArg any
	if orderID > 0 {
		orderArg = orderID
	}
	if adminID > 0 {
		adminArg = adminID
	}
	_, err := h.DB.Exec(ctx, `
		INSERT INTO user_block_log (user_id, action, reason, order_id, blocked_by, details)
		VALUES ($1, $2::user_block_action, $3, $4, $5, $6)
	`, userID, action, reason, orderArg, adminArg, details)
	if err != nil {
		h.Log.Warn("compliance log write failed",
			"err", err, "user_id", userID, "order_id", orderID, "action", action)
	}
}

// buildRedirectDialString matches the conventions /sipctl/authorize uses to
// produce the AUTH_ROUTE_TARGET arg the main dialplan dispatches on — same
// shape so anything that works at original route-time will work as a
// redirect target. Returns ("", err) if the inputs don't validate.
func buildRedirectDialString(routeKind, routeTarget, e164 string) (string, error) {
	if routeTarget == "" {
		return "", errors.New("empty target")
	}
	switch routeKind {
	case "sip_uri":
		if !validSIPURI(routeTarget) {
			return "", errors.New("sip_uri must look like sip:user@host[:port] or user@host")
		}
		t := routeTarget
		if !strings.HasPrefix(t, "sip:") && !strings.HasPrefix(t, "sips:") {
			t = "sip:" + t
		}
		return "PJSIP/outbound/" + t, nil
	case "ip":
		if !validIPTarget(routeTarget) {
			return "", errors.New("ip target must be ip[:port]")
		}
		return "PJSIP/outbound/sip:" + e164 + "@" + routeTarget, nil
	case "sip_account":
		if !validUsername(routeTarget) {
			return "", errors.New("sip_account must be 1-32 chars [a-zA-Z0-9_-]")
		}
		return "PJSIP/" + routeTarget, nil
	}
	return "", errors.New("unknown route_kind — expected sip_uri / ip / sip_account")
}

var (
	sipURIRE  = regexp.MustCompile(`^(sip|sips):?[^\s<>"]+@[^\s<>"]+$`)
	ipPortRE  = regexp.MustCompile(`^[0-9.]+(:[0-9]+)?$`)
	userRE    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)
)

func validSIPURI(s string) bool {
	if sipURIRE.MatchString(s) {
		return true
	}
	return strings.Contains(s, "@") && !strings.ContainsAny(s, " <>\"")
}
func validIPTarget(s string) bool { return ipPortRE.MatchString(s) }
func validUsername(s string) bool { return userRE.MatchString(s) }
