package resellerapi

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"didstorage/internal/asteriskcfg"
	"didstorage/internal/causes"
	"didstorage/internal/domain"
	"didstorage/internal/livecalls"
)

// This file extends the original resellerapi handlers with everything that
// was added to the admin GUI between checkpoints 0005 and 0007 but never
// reached the reseller-facing JSON surface.
//
// Data isolation is enforced in every query: a JOIN to users.reseller_id =
// caller_reseller, or an equivalent ownership check. The reseller API NEVER
// returns supplier IPs, supplier hostnames, supplier names, admin emails,
// other resellers' rows, audio-file basenames, or admin live-action metadata.

// ----------------------------------------------------------------------
// GET /me
// ----------------------------------------------------------------------

type meOut struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	BrandName    *string `json:"brand_name,omitempty"`
	Hostname     *string `json:"portal_hostname,omitempty"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"created_at"`
	UserCount    int     `json:"user_count"`
	ActiveOrders int     `json:"active_orders"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	var out meOut
	var brand, host *string
	var created time.Time
	err := h.DB.QueryRow(r.Context(), `
		SELECT r.id, r.name, r.brand_name, r.portal_hostname, r.status, r.created_at,
		       (SELECT count(*) FROM users  u WHERE u.reseller_id = r.id),
		       (SELECT count(*) FROM orders o JOIN users u ON u.id = o.user_id
		         WHERE u.reseller_id = r.id AND o.status = 'active')
		  FROM resellers r WHERE r.id = $1`, rid,
	).Scan(&out.ID, &out.Name, &brand, &host, &out.Status, &created, &out.UserCount, &out.ActiveOrders)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out.BrandName = brand
	out.Hostname = host
	out.CreatedAt = created.UTC().Format(time.RFC3339)
	writeJSON(w, 200, out)
}

// ----------------------------------------------------------------------
// GET /users/{id}/ledger
// ----------------------------------------------------------------------

type ledgerOut struct {
	ID           int64  `json:"id"`
	CreatedAt    string `json:"created_at"`
	DeltaCents   int64  `json:"delta_cents"`
	Kind         string `json:"kind"`
	BalanceAfter int64  `json:"balance_after_cents"`
	RefTable     string `json:"ref_table,omitempty"`
	RefID        int64  `json:"ref_id,omitempty"`
	RefCallID    string `json:"ref_call_id,omitempty"`
	RefOrderID   int64  `json:"ref_order_id,omitempty"`
}

func (h *Handler) listUserLedger(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 && n <= 1000 {
			limit = n
		}
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT l.id, to_char(l.created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       l.delta_cents, l.kind::text, l.balance_after,
		       COALESCE(l.ref_table, ''),
		       COALESCE(l.ref_id, 0),
		       CASE l.ref_table
		         WHEN 'cdrs' THEN COALESCE((SELECT c.call_id FROM cdrs c WHERE c.id = l.ref_id), '')
		         ELSE ''
		       END AS ref_call_id,
		       CASE l.ref_table
		         WHEN 'orders'       THEN COALESCE(l.ref_id, 0)
		         WHEN 'cdrs'         THEN COALESCE((SELECT c.order_id FROM cdrs c WHERE c.id = l.ref_id), 0)
		         WHEN 'billing_runs' THEN COALESCE((SELECT br.order_id FROM billing_runs br WHERE br.id = l.ref_id), 0)
		         ELSE 0
		       END AS ref_order_id
		  FROM balance_ledger l
		 WHERE l.user_id = $1
		 ORDER BY l.id DESC LIMIT $2`, uid, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []ledgerOut
	for rows.Next() {
		var e ledgerOut
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.DeltaCents, &e.Kind, &e.BalanceAfter,
			&e.RefTable, &e.RefID, &e.RefCallID, &e.RefOrderID); err == nil {
			// Sanitize call_id (some legacy rows store the raw "prefix@host:port").
			if e.RefCallID != "" {
				e.RefCallID = domain.SanitizeCallID(e.RefCallID)
			}
			out = append(out, e)
		}
	}
	writeJSON(w, 200, map[string]any{"ledger": out})
}

// ----------------------------------------------------------------------
// /users/{id}/sip-accounts (full CRUD)
// ----------------------------------------------------------------------
//
// The peer's `ha1` (Asterisk digest secret) is NEVER returned by GET. POST
// accepts a plaintext password and computes ha1 = md5(username:realm:password)
// server-side. DELETE drops the row. After every mutation we kick off a
// goroutine that runs asteriskcfg.RegenPJSIPUsers so Asterisk picks up the
// new endpoint without manual reload.

// sipRealm mirrors the constant in internal/web/crud.go. Kept in sync by
// convention; both packages must use the same realm so digest auth
// negotiates successfully across clients.
const sipRealm = "didstorage.local"

type sipAccountOut struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Realm     string `json:"realm"`
	CreatedAt string `json:"created_at"`
}

func (h *Handler) listSipAccounts(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, user_id, username, realm, to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		  FROM sip_accounts WHERE user_id = $1 ORDER BY id DESC`, uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []sipAccountOut
	for rows.Next() {
		var s sipAccountOut
		if err := rows.Scan(&s.ID, &s.UserID, &s.Username, &s.Realm, &s.CreatedAt); err == nil {
			out = append(out, s)
		}
	}
	writeJSON(w, 200, map[string]any{"sip_accounts": out})
}

// reservedSIPUsernames mirrors the admin-GUI set. Picking one of these as a
// peer username would collide with an Asterisk built-in section.
var reservedSIPUsernames = map[string]bool{
	"global": true, "system": true, "general": true, "default": true,
	"anonymous": true, "outbound": true, "transport-udp": true, "transport-tcp": true,
	"supplier-trunk": true, "from-account": true, "from-supplier": true,
	"hangup-handler": true, "admin-actions": true,
}

func validSIPUsername(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	if reservedSIPUsernames[strings.ToLower(s)] {
		return false
	}
	if strings.HasSuffix(s, "-auth") || strings.HasSuffix(s, "-aor") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func ha1(user, realm, pass string) string {
	sum := md5.Sum([]byte(user + ":" + realm + ":" + pass))
	return hex.EncodeToString(sum[:])
}

// pgErrCode returns the Postgres SQLSTATE for a wrapped pgx error, or "".
// Used to distinguish duplicate-key violations (23505) from generic failures.
func pgErrCode(err error) string {
	if err == nil {
		return ""
	}
	type sqlStater interface{ SQLState() string }
	var pgErr sqlStater
	if errors.As(err, &pgErr) {
		return pgErr.SQLState()
	}
	return ""
}

func (h *Handler) createSipAccount(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !validSIPUsername(req.Username) {
		writeErr(w, 400, "username must be 1-32 chars [a-zA-Z0-9_-]; no Asterisk-reserved names; no -auth/-aor suffix")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, 400, "password must be >= 8 chars")
		return
	}
	hashed := ha1(req.Username, sipRealm, req.Password)
	var id int64
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO sip_accounts (user_id, username, realm, ha1)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		uid, req.Username, sipRealm, hashed,
	).Scan(&id)
	if err != nil {
		if pgErrCode(err) == "23505" {
			writeErr(w, 409, "a SIP peer with this username already exists at this realm")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Push the new peer into Asterisk: write pjsip_users.conf + reload PJSIP.
	// Best-effort; the DB row is the source of truth and a subsequent admin
	// action will regenerate the file if this fails.
	go asteriskcfg.RegenPJSIPUsers(h.DB, h.Log)

	writeJSON(w, 201, map[string]any{
		"id":       id,
		"username": req.Username,
		"realm":    sipRealm,
	})
}

func (h *Handler) deleteSipAccount(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	said, _ := strconv.ParseInt(chi.URLParam(r, "said"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`DELETE FROM sip_accounts WHERE id = $1 AND user_id = $2`, said, uid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "sip account not found")
		return
	}
	go asteriskcfg.RegenPJSIPUsers(h.DB, h.Log)
	writeJSON(w, 200, map[string]any{"status": "deleted"})
}

// ----------------------------------------------------------------------
// PATCH /orders/{id}
// ----------------------------------------------------------------------

func (h *Handler) updateOrder(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	oid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req struct {
		RouteKind    *string `json:"route_kind,omitempty"`
		RouteTarget  *string `json:"route_target,omitempty"`
		ChannelCount *int    `json:"channel_count,omitempty"`
		KycBundleID  *int64  `json:"kyc_bundle_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}

	var curUser int64
	var curRK, curRT, curStatus string
	var curChannels int
	err := h.DB.QueryRow(r.Context(), `
		SELECT o.user_id, o.route_kind::text, o.route_target, o.channel_count, o.status::text
		  FROM orders o
		  JOIN users u ON u.id = o.user_id
		 WHERE o.id = $1 AND u.reseller_id = $2`, oid, rid,
	).Scan(&curUser, &curRK, &curRT, &curChannels, &curStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "order not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if curStatus != "active" && curStatus != "kyc_pending" && curStatus != "quarantined" {
		writeErr(w, 409, "order is not editable in state "+curStatus)
		return
	}

	rk := curRK
	if req.RouteKind != nil {
		rk = strings.TrimSpace(*req.RouteKind)
	}
	if rk != "sip_uri" && rk != "ip" && rk != "sip_account" {
		writeErr(w, 400, "route_kind must be sip_uri | ip | sip_account")
		return
	}
	rt := curRT
	if req.RouteTarget != nil {
		rt = strings.TrimSpace(*req.RouteTarget)
	}
	rt = domain.NormalizeRouteTarget(rk, rt)
	if rt == "" {
		writeErr(w, 400, "route_target required")
		return
	}
	ch := curChannels
	if req.ChannelCount != nil {
		ch = *req.ChannelCount
		if ch < 0 {
			writeErr(w, 400, "channel_count must be >= 0")
			return
		}
	}

	var bundleArg any
	if req.KycBundleID != nil && *req.KycBundleID > 0 {
		bid := *req.KycBundleID
		var bUser int64
		if err := h.DB.QueryRow(r.Context(),
			`SELECT user_id FROM kyc_bundles WHERE id = $1`, bid,
		).Scan(&bUser); err != nil || bUser != curUser {
			writeErr(w, 400, "invalid kyc_bundle_id for this user")
			return
		}
		bundleArg = bid
	}

	_, err = h.DB.Exec(r.Context(), `
		UPDATE orders o
		   SET route_kind   = $1::route_kind,
		       route_target = $2,
		       channel_count = $3,
		       kyc_bundle_id = COALESCE($4::bigint, kyc_bundle_id),
		       status = CASE
		           WHEN o.status = 'kyc_pending'
		                AND $4::bigint IS NOT NULL
		                AND EXISTS (SELECT 1 FROM kyc_bundles b
		                             WHERE b.id = $4 AND b.status = 'approved')
		             THEN 'active'::assignment_status
		           ELSE o.status
		         END
		 WHERE o.id = $5`,
		rk, rt, ch, bundleArg, oid)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"status":        "updated",
		"route_kind":    rk,
		"route_target":  rt,
		"channel_count": ch,
	})
}

// ----------------------------------------------------------------------
// GET /orders/{id}/cdrs
// ----------------------------------------------------------------------

func (h *Handler) listOrderCDRs(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	oid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var ok int
	err := h.DB.QueryRow(r.Context(), `
		SELECT 1 FROM orders o JOIN users u ON u.id=o.user_id
		 WHERE o.id=$1 AND u.reseller_id=$2`, oid, rid).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "order not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT c.id, c.call_id, d.e164, c.user_id, u.external_id, c.order_id,
		       c.started_at, c.ended_at,
		       c.billsec, c.charged_minutes, c.charge_cents, COALESCE(c.hangup_cause,'')
		  FROM cdrs c
		  JOIN users  u ON u.id = c.user_id
		  JOIN orders o ON o.id = c.order_id
		  JOIN dids   d ON d.id = o.did_id
		 WHERE c.order_id = $1
		 ORDER BY c.started_at DESC LIMIT $2`, oid, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type row struct {
		ID             int64   `json:"id"`
		CallID         string  `json:"call_id"`
		DID            string  `json:"did_e164"`
		UserID         int64   `json:"user_id"`
		UserExternalID *string `json:"user_external_id,omitempty"`
		OrderID        int64   `json:"order_id"`
		StartedAt      string  `json:"started_at"`
		EndedAt        string  `json:"ended_at"`
		Billsec        int     `json:"billsec"`
		ChargedMinutes int     `json:"charged_minutes"`
		ChargeCents    int     `json:"charge_cents"`
		HangupCause    string  `json:"hangup_cause"`
		State          string  `json:"state"`
	}
	var out []row
	for rows.Next() {
		var x row
		var s, e time.Time
		var ext *string
		if err := rows.Scan(&x.ID, &x.CallID, &x.DID, &x.UserID, &ext, &x.OrderID,
			&s, &e, &x.Billsec, &x.ChargedMinutes, &x.ChargeCents, &x.HangupCause); err == nil {
			x.CallID = domain.SanitizeCallID(x.CallID)
			x.UserExternalID = ext
			x.StartedAt = s.UTC().Format(time.RFC3339)
			x.EndedAt = e.UTC().Format(time.RFC3339)
			x.State = domain.CallState(x.Billsec, x.HangupCause)
			out = append(out, x)
		}
	}
	writeJSON(w, 200, map[string]any{"cdrs": out})
}

// ----------------------------------------------------------------------
// GET /live
// ----------------------------------------------------------------------
//
// Active inbound calls for this reseller, derived from the Redis live-call
// index. Reseller can see: call_id (sanitized), DID, user_id, order_id,
// source caller (URI cleaned to bare number), routed target, age. Never:
// supplier_id, source IP, asterisk_channel, admin_action.

type liveOut struct {
	CallID        string  `json:"call_id"`
	E164          string  `json:"did_e164"`
	UserID        int64   `json:"user_id"`
	OrderID       int64   `json:"order_id,omitempty"`
	SrcURI        string  `json:"src_uri,omitempty"`
	RouteKind     string  `json:"route_kind,omitempty"`
	RouteTarget   string  `json:"route_target,omitempty"`
	StartedAt     string  `json:"started_at"`
	BillsecApprox int     `json:"billsec_approx"`
	RatePerMin    float64 `json:"rate_cents_per_min,omitempty"`
}

func (h *Handler) listLive(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	if h.Redis == nil {
		writeJSON(w, 200, map[string]any{"calls": []liveOut{}})
		return
	}
	calls, err := livecalls.List(r.Context(), h.Redis)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(calls) == 0 {
		writeJSON(w, 200, map[string]any{"calls": []liveOut{}})
		return
	}
	// Resolve which user belongs to which reseller in one query.
	userIDs := make([]int64, 0, len(calls))
	seen := map[int64]bool{}
	for _, c := range calls {
		if c.UserID > 0 && !seen[c.UserID] {
			userIDs = append(userIDs, c.UserID)
			seen[c.UserID] = true
		}
	}
	resellerOf := map[int64]int64{}
	if len(userIDs) > 0 {
		rows, qerr := h.DB.Query(r.Context(),
			`SELECT id, COALESCE(reseller_id, 0) FROM users WHERE id = ANY($1::bigint[])`, userIDs)
		if qerr == nil {
			for rows.Next() {
				var uid, rs int64
				if err := rows.Scan(&uid, &rs); err == nil {
					resellerOf[uid] = rs
				}
			}
			rows.Close()
		}
	}
	now := time.Now()
	var visible []liveOut
	for _, c := range calls {
		if c.Reserved {
			continue // admin-only reservation traffic
		}
		if resellerOf[c.UserID] != rid {
			continue
		}
		startedAt := time.Unix(c.StartedAt, 0).UTC()
		visible = append(visible, liveOut{
			CallID:        domain.SanitizeCallID(c.CallID),
			E164:          c.E164,
			UserID:        c.UserID,
			OrderID:       c.OrderID,
			SrcURI:        domain.CleanCallerURI(c.SrcURI),
			RouteKind:     c.RouteKind,
			RouteTarget:   c.RouteTarget,
			StartedAt:     startedAt.Format(time.RFC3339),
			BillsecApprox: int(now.Sub(startedAt).Seconds()),
			RatePerMin:    c.RatePerMinCents,
		})
	}
	writeJSON(w, 200, map[string]any{"calls": visible})
}

// ----------------------------------------------------------------------
// GET /cause-codes
// ----------------------------------------------------------------------
//
// Read-only mirror of the platform's hangup_causes table. Lets resellers'
// dashboards render the same friendly label + tooltip detail the admin GUI
// shows for each CDR cause cell.

func (h *Handler) listCauseCodes(w http.ResponseWriter, r *http.Request) {
	type out struct {
		Code   string `json:"code"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
		Family string `json:"family"`
	}
	all := causes.All()
	resp := make([]out, 0, len(all))
	for _, c := range all {
		resp = append(resp, out{
			Code:   c.Code,
			Label:  c.Label,
			Detail: c.Detail,
			Family: c.Family,
		})
	}
	writeJSON(w, 200, map[string]any{"cause_codes": resp})
}
