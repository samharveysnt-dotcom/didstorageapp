// Package resellerapi exposes the JSON REST API resellers use to manage their
// own users, orders, DIDs, and CDRs. Authenticated via bearer-token API keys;
// every query is scoped to the reseller_id derived from the key.
//
// Domain model:
//   - user  = customer the reseller is representing (login-less; balance, KYC,
//             channel cap)
//   - order = per-DID rental owned by a user (one DID, channels, route,
//             anniversary billing)
package resellerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"didstorage/internal/db"
	"didstorage/internal/domain"
	"didstorage/internal/siptrace"
)

// callIDSearchPattern returns a SQL LIKE pattern that matches both the
// sanitized prefix and the full "prefix@host:port" form. Resellers may pass
// either in URL paths.
func callIDSearchPattern(s string) string {
	return domain.SanitizeCallID(s)
}

type Handler struct {
	DB       *db.DB
	Redis    *redis.Client // optional; nil disables /live
	Log      *slog.Logger
	PublicIP string
}

func New(pg *db.DB, rdb *redis.Client, log *slog.Logger, publicIP string) *Handler {
	return &Handler{DB: pg, Redis: rdb, Log: log, PublicIP: publicIP}
}

type ctxKey string

const ctxResellerKey ctxKey = "reseller_id"

// Mount attaches /api/v1/* routes under the given chi router.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(h.bearerAuth)

		// Reseller self
		r.Get("/me", h.me)

		// Users (customer-level)
		r.Get("/users", h.listUsers)
		r.Post("/users", h.createUser)
		r.Get("/users/{id}", h.getUser)
		r.Get("/users/by-external/{ext}", h.getUserByExternal)
		r.Post("/users/{id}/topup", h.topupUser)
		r.Get("/users/{id}/ledger", h.listUserLedger)
		r.Get("/users/{id}/sip-accounts", h.listSipAccounts)
		r.Post("/users/{id}/sip-accounts", h.createSipAccount)
		r.Delete("/users/{id}/sip-accounts/{said}", h.deleteSipAccount)

		// Orders (per-DID rental)
		r.Get("/orders", h.listOrders)
		r.Post("/orders", h.createOrder)
		r.Get("/orders/{id}", h.getOrder)
		r.Patch("/orders/{id}", h.updateOrder)
		r.Post("/orders/{id}/cancel", h.cancelOrder)
		r.Get("/orders/{id}/cdrs", h.listOrderCDRs)

		// DIDs (catalog + reseller's assigned)
		r.Get("/dids", h.listDIDs)

		// CDRs and call traces
		r.Get("/cdrs", h.listCDRs)
		r.Get("/cdrs/{call_id}/sip-trace", h.cdrSipTrace)

		// Live calls (sanitized for reseller scope)
		r.Get("/live", h.listLive)

		// Hangup-cause metadata (read-only)
		r.Get("/cause-codes", h.listCauseCodes)

		// KYC bundles + documents
		r.Post("/users/{id}/kyc-bundles", h.createKycBundle)
		r.Get("/users/{id}/kyc-bundles", h.listKycBundles)
		r.Get("/kyc-bundles/{bid}", h.getKycBundle)
		r.Post("/kyc-bundles/{bid}/documents", h.uploadKycDocument)
		r.Get("/kyc-bundles/{bid}/documents/{did}/download", h.downloadKycDocument)

		// Compliance CSV exports
		r.Get("/users/{id}/export/cdrs.csv", h.exportUserCDRs)
		r.Get("/users/{id}/export/ledger.csv", h.exportUserLedger)
		r.Get("/orders/{id}/export/cdrs.csv", h.exportOrderCDRs)
	})
}

// ----------------------------------------------------------------------
// auth
// ----------------------------------------------------------------------

func (h *Handler) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := bearerToken(r.Header.Get("Authorization"))
		if secret == "" {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		rows, err := h.DB.Query(r.Context(),
			`SELECT id, reseller_id, key_hash FROM api_keys WHERE revoked_at IS NULL`)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "auth lookup failed")
			return
		}
		var matchKeyID, matchReseller int64
		for rows.Next() {
			var id int64
			var rid *int64
			var hash string
			if err := rows.Scan(&id, &rid, &hash); err != nil {
				continue
			}
			if rid == nil {
				continue
			}
			if bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil {
				matchKeyID = id
				matchReseller = *rid
				break
			}
		}
		rows.Close()
		if matchKeyID == 0 {
			writeErr(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		go func() {
			_, _ = h.DB.Exec(context.Background(),
				`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, matchKeyID)
		}()

		ctx := context.WithValue(r.Context(), ctxResellerKey, matchReseller)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(h string) string {
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

func resellerID(ctx context.Context) int64 {
	v, _ := ctx.Value(ctxResellerKey).(int64)
	return v
}

// ----------------------------------------------------------------------
// users
// ----------------------------------------------------------------------

type userOut struct {
	ID           int64   `json:"id"`
	ExternalID   *string `json:"external_id,omitempty"`
	Label        *string `json:"label,omitempty"`
	ContactEmail *string `json:"contact_email,omitempty"`
	Status       string  `json:"status"`
	BalanceCents int64   `json:"balance_cents"`
	ChannelCap   *int    `json:"global_channel_cap,omitempty"`
	CreatedAt    string  `json:"created_at"`
	ActiveOrders int     `json:"active_orders"`
}

func scanUser(rows interface {
	Scan(...any) error
}) (userOut, error) {
	var u userOut
	var ext, label, contact *string
	var capN *int
	var created time.Time
	if err := rows.Scan(&u.ID, &ext, &label, &contact, &u.Status, &u.BalanceCents, &capN, &created, &u.ActiveOrders); err != nil {
		return u, err
	}
	u.ExternalID = ext
	u.Label = label
	u.ContactEmail = contact
	u.ChannelCap = capN
	u.CreatedAt = created.UTC().Format(time.RFC3339)
	return u, nil
}

const userSelectCols = `u.id, u.external_id, u.label, u.contact_email, u.status,
    u.balance_cents, u.global_channel_cap, u.created_at,
    (SELECT count(*) FROM orders WHERE user_id=u.id AND status='active')`

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	rows, err := h.DB.Query(r.Context(),
		`SELECT `+userSelectCols+` FROM users u WHERE u.reseller_id = $1 ORDER BY u.id DESC LIMIT 500`,
		rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var out []userOut
	for rows.Next() {
		u, err := scanUser(rows)
		if err == nil {
			out = append(out, u)
		}
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"users": out})
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	var req struct {
		ExternalID       string `json:"external_id"`
		Label            string `json:"label"`
		ContactEmail     string `json:"contact_email"`
		GlobalChannelCap *int   `json:"global_channel_cap,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.ExternalID == "" {
		writeErr(w, 400, "external_id is required (it's how you'll find this user in your system)")
		return
	}
	var labelPtr, contactPtr *string
	if req.Label != "" {
		labelPtr = &req.Label
	}
	if req.ContactEmail != "" {
		contactPtr = &req.ContactEmail
	}
	var id int64
	err := h.DB.QueryRow(r.Context(), `
		INSERT INTO users (external_id, label, contact_email, balance_cents, reseller_id,
		                   global_channel_cap, status)
		VALUES ($1, $2, $3, 0, $4, $5, 'active') RETURNING id`,
		req.ExternalID, labelPtr, contactPtr, rid, req.GlobalChannelCap).Scan(&id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "external_id": req.ExternalID})
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	rows, err := h.DB.Query(r.Context(),
		`SELECT `+userSelectCols+` FROM users u WHERE u.id=$1 AND u.reseller_id=$2`,
		id, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeErr(w, 404, "user not found")
		return
	}
	u, err := scanUser(rows)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, u)
}

func (h *Handler) getUserByExternal(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	ext := chi.URLParam(r, "ext")
	rows, err := h.DB.Query(r.Context(),
		`SELECT `+userSelectCols+` FROM users u WHERE u.external_id=$1 AND u.reseller_id=$2`,
		ext, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeErr(w, 404, "user not found")
		return
	}
	u, err := scanUser(rows)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, u)
}

func (h *Handler) topupUser(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req struct {
		AmountCents int64 `json:"amount_cents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if req.AmountCents <= 0 {
		writeErr(w, 400, "amount_cents must be > 0")
		return
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	var bal int64
	err = tx.QueryRow(r.Context(),
		`UPDATE users SET balance_cents = balance_cents + $1
		   WHERE id=$2 AND reseller_id=$3 RETURNING balance_cents`,
		req.AmountCents, id, rid).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "user not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO balance_ledger (user_id, delta_cents, kind, balance_after) VALUES ($1,$2,'topup',$3)`,
		id, req.AmountCents, bal); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"balance_cents": bal})
}

// ----------------------------------------------------------------------
// orders (per-DID rentals)
// ----------------------------------------------------------------------

type orderOut struct {
	ID            int64   `json:"id"`
	UserID        int64   `json:"user_id"`
	DIDID         int64   `json:"did_id"`
	DID           string  `json:"did_e164"`
	Status        string  `json:"status"`
	ChannelCount  int     `json:"channel_count"`
	RouteKind     string  `json:"route_kind"`
	RouteTarget   string  `json:"route_target"`
	NextBillingAt string  `json:"next_billing_at"`
	KycBundleID   *int64  `json:"kyc_bundle_id,omitempty"`
	KycStatus     *string `json:"kyc_status,omitempty"`
}

const orderSelectCols = `o.id, o.user_id, o.did_id, d.e164, o.status::text,
    o.channel_count, o.route_kind::text, o.route_target, o.next_billing_at,
    o.kyc_bundle_id,
    (SELECT b.status::text FROM kyc_bundles b WHERE b.id = o.kyc_bundle_id)`

func scanOrder(rows interface {
	Scan(...any) error
}) (orderOut, error) {
	var o orderOut
	var next time.Time
	var bundleID *int64
	var kycStatus *string
	if err := rows.Scan(&o.ID, &o.UserID, &o.DIDID, &o.DID, &o.Status,
		&o.ChannelCount, &o.RouteKind, &o.RouteTarget, &next, &bundleID, &kycStatus); err != nil {
		return o, err
	}
	o.NextBillingAt = next.UTC().Format(time.RFC3339)
	o.KycBundleID = bundleID
	o.KycStatus = kycStatus
	return o, nil
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	rows, err := h.DB.Query(r.Context(), `
		SELECT `+orderSelectCols+`
		  FROM orders o
		  JOIN dids  d ON d.id = o.did_id
		  JOIN users u ON u.id = o.user_id
		 WHERE u.reseller_id = $1
		 ORDER BY o.id DESC LIMIT 500`, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var out []orderOut
	for rows.Next() {
		o, err := scanOrder(rows)
		if err == nil {
			out = append(out, o)
		}
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"orders": out})
}

// createOrder creates a per-DID rental for one of the reseller's users.
// If a kyc_bundle_id is supplied and approved, the order goes 'active';
// otherwise it goes 'kyc_pending'. NRC charges from the user's balance.
func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	var req struct {
		UserID         int64  `json:"user_id"`
		DIDID          int64  `json:"did_id"`
		ChannelCount   int    `json:"channel_count"`
		RouteKind      string `json:"route_kind"`
		RouteTarget    string `json:"route_target"`
		AnniversaryDay int    `json:"anniversary_day"`
		KycBundleID    *int64 `json:"kyc_bundle_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if req.ChannelCount < 0 {
		req.ChannelCount = 0
	}
	if req.AnniversaryDay < 1 || req.AnniversaryDay > 28 {
		now := time.Now().UTC().Day()
		if now > 28 {
			now = 28
		}
		req.AnniversaryDay = now
	}
	if req.RouteKind == "" || req.RouteTarget == "" {
		writeErr(w, 400, "route_kind and route_target required")
		return
	}
	req.RouteTarget = domain.NormalizeRouteTarget(req.RouteKind, req.RouteTarget)

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	// Verify the user belongs to this reseller and grab balance.
	var ownedRID *int64
	var bal int64
	if err := tx.QueryRow(r.Context(),
		`SELECT reseller_id, balance_cents FROM users WHERE id=$1 FOR UPDATE`, req.UserID,
	).Scan(&ownedRID, &bal); err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	if ownedRID == nil || *ownedRID != rid {
		writeErr(w, 403, "user does not belong to your reseller")
		return
	}

	var didStatus string
	if err := tx.QueryRow(r.Context(),
		`SELECT status FROM dids WHERE id=$1 FOR UPDATE`, req.DIDID,
	).Scan(&didStatus); err != nil {
		writeErr(w, 404, "did not found")
		return
	}
	if didStatus != "available" {
		writeErr(w, 409, "did not available")
		return
	}

	var rateCardID int64
	var nrc int
	err = tx.QueryRow(r.Context(), `
		SELECT rc.id, rc.nrc_cents
		  FROM dids d
		  JOIN rate_cards rc ON rc.supplier_id=d.supplier_id
		                    AND rc.country_iso=d.country_iso
		                    AND rc.did_type=d.did_type
		                    AND rc.valid_to IS NULL
		 WHERE d.id=$1
		 ORDER BY rc.valid_from DESC LIMIT 1`, req.DIDID).Scan(&rateCardID, &nrc)
	if err != nil {
		writeErr(w, 409, "no active rate card for this DID")
		return
	}

	if bal < int64(nrc) {
		writeErr(w, 402, fmt.Sprintf("insufficient balance: NRC %d cents > balance %d cents", nrc, bal))
		return
	}

	// If a KYC bundle is supplied, verify and decide initial status.
	startStatus := "kyc_pending"
	if req.KycBundleID != nil {
		var bUser int64
		var bStatus string
		err := tx.QueryRow(r.Context(),
			`SELECT user_id, status::text FROM kyc_bundles WHERE id=$1`, *req.KycBundleID,
		).Scan(&bUser, &bStatus)
		if err != nil || bUser != req.UserID {
			writeErr(w, 400, "invalid kyc_bundle_id")
			return
		}
		if bStatus == "approved" {
			startStatus = "active"
		}
	}

	now := time.Now().UTC()
	next := domain.NextAnniversary(now, req.AnniversaryDay)
	var oid int64
	err = tx.QueryRow(r.Context(), `
		INSERT INTO orders (did_id, user_id, channel_count, route_kind, route_target,
		                    rate_card_id, anniversary_day, next_billing_at, status, kyc_bundle_id)
		VALUES ($1,$2,$3,$4::route_kind,$5,$6,$7,$8,$9::assignment_status,$10) RETURNING id`,
		req.DIDID, req.UserID, req.ChannelCount, req.RouteKind, req.RouteTarget,
		rateCardID, req.AnniversaryDay, next, startStatus, req.KycBundleID).Scan(&oid)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE dids SET status='assigned' WHERE id=$1`, req.DIDID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if nrc > 0 {
		newBal := bal - int64(nrc)
		tx.Exec(r.Context(), `UPDATE users SET balance_cents=$1 WHERE id=$2`, newBal, req.UserID)
		tx.Exec(r.Context(),
			`INSERT INTO balance_ledger (user_id, delta_cents, kind, ref_table, ref_id, balance_after)
			 VALUES ($1,$2,'nrc','orders',$3,$4)`,
			req.UserID, -int64(nrc), oid, newBal)
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"order_id":          oid,
		"status":            startStatus,
		"nrc_charged_cents": nrc,
	})
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	rows, err := h.DB.Query(r.Context(), `
		SELECT `+orderSelectCols+`
		  FROM orders o
		  JOIN dids  d ON d.id = o.did_id
		  JOIN users u ON u.id = o.user_id
		 WHERE o.id = $1 AND u.reseller_id = $2`, id, rid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	if !rows.Next() {
		writeErr(w, 404, "order not found")
		return
	}
	o, err := scanOrder(rows)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, o)
}

func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	oid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	var didID int64
	err = tx.QueryRow(r.Context(), `
		UPDATE orders o SET status='cancelled', ended_at=now()
		  FROM users u
		 WHERE o.id = $1 AND o.user_id = u.id AND u.reseller_id = $2
		   AND o.status IN ('active','kyc_pending','quarantined','suspended')
		 RETURNING o.did_id`, oid, rid).Scan(&didID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "order not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `UPDATE dids SET status='available' WHERE id=$1`, didID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"status": "cancelled"})
}

// ----------------------------------------------------------------------
// dids
// ----------------------------------------------------------------------

type didOut struct {
	ID          int64   `json:"id"`
	E164        string  `json:"e164"`
	Country     string  `json:"country_iso"`
	Type        string  `json:"did_type"`
	Status      string  `json:"status"`
	UserID      *int64  `json:"user_id,omitempty"`
	OrderID     *int64  `json:"order_id,omitempty"`
	Channels    *int    `json:"channel_count,omitempty"`
	RouteKind   *string `json:"route_kind,omitempty"`
	RouteTarget *string `json:"route_target,omitempty"`
}

// listDIDs returns DIDs visible to the reseller: every globally-available DID
// (the catalog) plus every DID currently assigned to one of the reseller's
// users.
//
// Query params:
//
//	country=GB           filter by ISO country code (exact match, case-insensitive)
//	did_type=mobile      filter by did_type enum value (mobile|national|local|tollfree)
//	available=true       only return globally-available DIDs (skip assigned)
//	q=4420               substring match on E.164
//	limit=200            cap rows (default 1000, max 5000)
func (h *Handler) listDIDs(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	q := r.URL.Query()

	limit := 1000
	if v := q.Get("limit"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 && n <= 5000 {
			limit = n
		}
	}

	sql := `
		SELECT d.id, d.e164, d.country_iso, d.did_type::text, d.status,
		       o.user_id, o.id, o.channel_count, o.route_kind::text, o.route_target
		  FROM dids d
		  LEFT JOIN orders o ON o.did_id = d.id AND o.status IN ('active','kyc_pending','quarantined')
		  LEFT JOIN users  u ON u.id = o.user_id
		 WHERE (`
	args := []any{rid}

	availOnly := strings.EqualFold(q.Get("available"), "true") || q.Get("available") == "1"
	if availOnly {
		sql += `d.status = 'available'`
	} else {
		sql += `d.status = 'available' OR u.reseller_id = $1`
	}
	sql += `)`

	if v := strings.TrimSpace(q.Get("country")); v != "" {
		args = append(args, strings.ToUpper(v))
		sql += fmt.Sprintf(" AND d.country_iso = $%d", len(args))
	}
	if v := strings.TrimSpace(q.Get("did_type")); v != "" {
		switch v {
		case "mobile", "national", "local", "tollfree":
			args = append(args, v)
			sql += fmt.Sprintf(" AND d.did_type = $%d::did_type", len(args))
		default:
			writeErr(w, 400, "did_type must be one of: mobile | national | local | tollfree")
			return
		}
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		args = append(args, "%"+v+"%")
		sql += fmt.Sprintf(" AND d.e164 LIKE $%d", len(args))
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY d.e164 LIMIT $%d", len(args))

	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []didOut
	for rows.Next() {
		var d didOut
		var ch *int
		var rk, rt *string
		var uid, oid *int64
		if err := rows.Scan(&d.ID, &d.E164, &d.Country, &d.Type, &d.Status, &uid, &oid, &ch, &rk, &rt); err == nil {
			d.UserID = uid
			d.OrderID = oid
			d.Channels = ch
			d.RouteKind = rk
			d.RouteTarget = rt
			out = append(out, d)
		}
	}
	writeJSON(w, 200, map[string]any{"dids": out})
}

// ----------------------------------------------------------------------
// cdrs
// ----------------------------------------------------------------------

func (h *Handler) listCDRs(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, _ := strconv.Atoi(v); n > 0 && n <= 500 {
			limit = n
		}
	}

	sql := `
		SELECT c.id, c.call_id, d.e164, c.user_id, u.external_id, c.order_id,
		       c.started_at, c.ended_at,
		       c.billsec, c.charged_minutes, c.charge_cents,
		       c.bill_min_seconds, c.bill_increment_seconds,
		       c.hangup_cause
		  FROM cdrs c
		  JOIN users  u ON u.id = c.user_id
		  JOIN orders o ON o.id = c.order_id
		  JOIN dids   d ON d.id = o.did_id
		 WHERE u.reseller_id = $1`
	args := []any{rid}
	add := func(clause string, v any) {
		args = append(args, v)
		sql += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}
	if v := q.Get("user_id"); v != "" {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("c.user_id =", n)
		}
	}
	if v := q.Get("order_id"); v != "" {
		if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("c.order_id =", n)
		}
	}
	if v := strings.TrimSpace(q.Get("external_id")); v != "" {
		add("u.external_id =", v)
	}
	if v := strings.TrimSpace(q.Get("did")); v != "" {
		add("d.e164 LIKE", "%"+v+"%")
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			add("c.started_at >=", t)
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			add("c.started_at <", t.Add(24*time.Hour))
		}
	}
	switch q.Get("state") {
	case "answered":
		sql += " AND c.billsec > 0"
	case "failed":
		sql += " AND c.billsec = 0"
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY c.started_at DESC LIMIT $%d", len(args))

	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type row struct {
		ID             int64   `json:"id"`
		CallID         string  `json:"call_id"`
		DID            string  `json:"did_e164"`
		UserID         int64   `json:"user_id"`
		UserExternalID *string `json:"user_external_id,omitempty"`
		OrderID        *int64  `json:"order_id,omitempty"`
		StartedAt      string  `json:"started_at"`
		EndedAt        string  `json:"ended_at"`
		Billsec        int     `json:"billsec"`
		ChargedMinutes int     `json:"charged_minutes"`
		ChargeCents    int     `json:"charge_cents"`
		// Customer-side billing increment that was applied to this CDR.
		// Snapshotted at call time — never null for new CDRs, may be null
		// for legacy rows written before migration 0017.
		BillMinSeconds *int   `json:"bill_min_seconds,omitempty"`
		BillIncSeconds *int   `json:"bill_increment_seconds,omitempty"`
		HangupCause    string `json:"hangup_cause"`
		State          string `json:"state"`
	}
	var out []row
	for rows.Next() {
		var x row
		var s, e time.Time
		var hc, ext *string
		var oid *int64
		if err := rows.Scan(&x.ID, &x.CallID, &x.DID, &x.UserID, &ext, &oid, &s, &e,
			&x.Billsec, &x.ChargedMinutes, &x.ChargeCents,
			&x.BillMinSeconds, &x.BillIncSeconds,
			&hc); err == nil {
			// Defense-in-depth: sanitize at output too. Newly-written CDRs
			// already store the prefix; pre-sanitization legacy rows still
			// contain @ip:port which we don't want resellers to see.
			x.CallID = domain.SanitizeCallID(x.CallID)
			x.UserExternalID = ext
			x.OrderID = oid
			x.StartedAt = s.UTC().Format(time.RFC3339)
			x.EndedAt = e.UTC().Format(time.RFC3339)
			if hc != nil {
				x.HangupCause = *hc
			}
			x.State = domain.CallState(x.Billsec, x.HangupCause)
			out = append(out, x)
		}
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"cdrs": out})
}

// cdrSipTrace returns SIP messages observed on the wire for a given call_id.
// Scoped to the caller's reseller and sanitized:
//   - All supplier IPs (and supplier_ip:port pairs) are rewritten to the DID
//     number the customer is paying for. They never see which carrier the
//     traffic came from.
//   - Our public IP (and ip:5060) is rewritten to the reseller's brand_name.
//
// The customer pasted call_id may be sanitized prefix or raw form; siptrace
// matches by prefix so either works.
func (h *Handler) cdrSipTrace(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	rawCallID := chi.URLParam(r, "call_id")
	if rawCallID == "" {
		writeErr(w, 400, "call_id required")
		return
	}
	callIDPrefix := callIDSearchPattern(rawCallID)

	var cdrID int64
	var didE164, brandName string
	err := h.DB.QueryRow(r.Context(), `
		SELECT c.id, d.e164, COALESCE(re.brand_name, re.name)
		  FROM cdrs c
		  JOIN users  u ON u.id = c.user_id
		  JOIN orders o ON o.id = c.order_id
		  JOIN dids   d ON d.id = o.did_id
		  LEFT JOIN resellers re ON re.id = u.reseller_id
		 WHERE c.call_id = $1 AND u.reseller_id = $2`, callIDPrefix, rid,
	).Scan(&cdrID, &didE164, &brandName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, 404, "cdr not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	tr, err := siptrace.Lookup(r.Context(), callIDPrefix, h.PublicIP)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// Build the rewrite map. Supplier IPs come from supplier_ip_group_members;
	// our IP comes from h.PublicIP.
	rewrites := map[string]string{
		h.PublicIP + ":5060": brandName,
		h.PublicIP:           brandName,
	}
	srows, err := h.DB.Query(r.Context(), `
		SELECT host(m.cidr) FROM supplier_ip_group_members m
		 WHERE family(m.cidr) = 4`)
	if err == nil {
		didLabel := "did:" + didE164
		for srows.Next() {
			var ip string
			if err := srows.Scan(&ip); err == nil {
				rewrites[ip+":5060"] = didLabel
				rewrites[ip] = didLabel
			}
		}
		srows.Close()
	}
	tr.Sanitize(siptrace.Sanitization{IPRewrites: rewrites})
	writeJSON(w, 200, tr)
}

// ----------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
