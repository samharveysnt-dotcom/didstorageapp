package resellerapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// CSV exports for reseller-side compliance / accounting. Same shape as the
// admin GUI exports but always scoped to the reseller's data.
//
//	GET /api/v1/users/{id}/export/cdrs.csv
//	GET /api/v1/users/{id}/export/ledger.csv
//	GET /api/v1/orders/{id}/export/cdrs.csv

func resellerDateRange(r *http.Request) (from, to time.Time, hasFrom, hasTo bool) {
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = t
			hasFrom = true
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = t.Add(24 * time.Hour)
			hasTo = true
		}
	}
	return
}

func resellerCSVHeader(w http.ResponseWriter, filename string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return csv.NewWriter(w)
}

func (h *Handler) exportUserCDRs(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	from, to, hasFrom, hasTo := resellerDateRange(r)
	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       to_char(c.ended_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(d.e164, ''), COALESCE(c.order_id, 0),
		       c.billsec, c.charged_minutes,
		       c.rate_cents_per_min, c.charge_cents,
		       COALESCE(c.hangup_cause,''),
		       COALESCE(c.src_uri,''),
		       COALESCE(c.dst_uri,''),
		       COALESCE(c.routed_kind::text, COALESCE(o.route_kind::text,'')) AS fdt,
		       COALESCE(c.routed_target,    COALESCE(o.route_target,''))      AS fd
		  FROM cdrs c
		  LEFT JOIN orders o ON o.id = c.order_id
		  LEFT JOIN dids   d ON d.id = COALESCE(c.did_id, o.did_id)
		 WHERE c.user_id = $1`
	args := []any{uid}
	if hasFrom {
		args = append(args, from)
		sql += fmt.Sprintf(" AND c.started_at >= $%d", len(args))
	}
	if hasTo {
		args = append(args, to)
		sql += fmt.Sprintf(" AND c.started_at < $%d", len(args))
	}
	sql += ` ORDER BY c.started_at`
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	cw := resellerCSVHeader(w, fmt.Sprintf("cdrs-user-%d.csv", uid))
	cw.Write([]string{"cdr_id", "call_id", "started_at", "ended_at", "did_e164",
		"order_id", "billsec", "charged_minutes", "rate_cents_per_min", "charge_cents",
		"hangup_cause", "src", "dst", "final_dst", "final_dst_type"})
	for rows.Next() {
		var id, orderID int64
		var callID, started, ended, did, hc, src, dst, fdt, fd string
		var billsec, mins, charge int
		var rate float64
		if err := rows.Scan(&id, &callID, &started, &ended, &did, &orderID,
			&billsec, &mins, &rate, &charge, &hc, &src, &dst, &fdt, &fd); err != nil {
			continue
		}
		if fdt == "sip_account" && fd != "" && h.PublicIP != "" {
			fd = fd + "@" + h.PublicIP
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), callID, started, ended, did,
			strconv.FormatInt(orderID, 10),
			strconv.Itoa(billsec), strconv.Itoa(mins),
			strconv.FormatFloat(rate, 'f', 4, 64),
			strconv.Itoa(charge),
			hc, src, dst, fd, fdt,
		})
	}
	cw.Flush()
}

func (h *Handler) exportUserLedger(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	uid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if !h.userBelongsToReseller(w, r, uid, rid) {
		return
	}
	from, to, hasFrom, hasTo := resellerDateRange(r)
	sql := `
		SELECT id, to_char(created_at,'YYYY-MM-DD HH24:MI:SS'),
		       delta_cents, kind::text,
		       COALESCE(ref_table,''), ref_id, balance_after
		  FROM balance_ledger
		 WHERE user_id=$1`
	args := []any{uid}
	if hasFrom {
		args = append(args, from)
		sql += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if hasTo {
		args = append(args, to)
		sql += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	sql += ` ORDER BY id`
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	cw := resellerCSVHeader(w, fmt.Sprintf("ledger-user-%d.csv", uid))
	cw.Write([]string{"id", "created_at", "delta_cents", "kind",
		"ref_table", "ref_id", "balance_after"})
	for rows.Next() {
		var id, balanceAfter int64
		var refID *int64
		var created, kind, refTable string
		var delta int64
		if err := rows.Scan(&id, &created, &delta, &kind, &refTable, &refID, &balanceAfter); err != nil {
			continue
		}
		ref := ""
		if refID != nil {
			ref = strconv.FormatInt(*refID, 10)
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), created,
			strconv.FormatInt(delta, 10), kind,
			refTable, ref,
			strconv.FormatInt(balanceAfter, 10),
		})
	}
	cw.Flush()
}

func (h *Handler) exportOrderCDRs(w http.ResponseWriter, r *http.Request) {
	rid := resellerID(r.Context())
	oid, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)

	// Verify the order belongs to this reseller.
	var rows int
	err := h.DB.QueryRow(r.Context(), `
		SELECT 1 FROM orders o JOIN users u ON u.id=o.user_id
		 WHERE o.id=$1 AND u.reseller_id=$2`, oid, rid).Scan(&rows)
	if err != nil {
		http.Error(w, "order not found", 404)
		return
	}

	from, to, hasFrom, hasTo := resellerDateRange(r)
	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       to_char(c.ended_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(d.e164, ''), COALESCE(c.user_id, 0),
		       c.billsec, c.charged_minutes,
		       c.rate_cents_per_min, c.charge_cents,
		       COALESCE(c.hangup_cause,''),
		       COALESCE(c.src_uri,''),
		       COALESCE(c.dst_uri,''),
		       COALESCE(c.routed_kind::text, COALESCE(o.route_kind::text,'')) AS fdt,
		       COALESCE(c.routed_target,    COALESCE(o.route_target,''))      AS fd
		  FROM cdrs c
		  LEFT JOIN orders o ON o.id = c.order_id
		  LEFT JOIN dids   d ON d.id = COALESCE(c.did_id, o.did_id)
		 WHERE c.order_id = $1`
	args := []any{oid}
	if hasFrom {
		args = append(args, from)
		sql += fmt.Sprintf(" AND c.started_at >= $%d", len(args))
	}
	if hasTo {
		args = append(args, to)
		sql += fmt.Sprintf(" AND c.started_at < $%d", len(args))
	}
	sql += ` ORDER BY c.started_at`
	rrows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rrows.Close()
	cw := resellerCSVHeader(w, fmt.Sprintf("cdrs-order-%d.csv", oid))
	cw.Write([]string{"cdr_id", "call_id", "started_at", "ended_at", "did_e164",
		"user_id", "billsec", "charged_minutes", "rate_cents_per_min", "charge_cents",
		"hangup_cause", "src", "dst", "final_dst", "final_dst_type"})
	for rrows.Next() {
		var id, userID int64
		var callID, started, ended, did, hc, src, dst, fdt, fd string
		var billsec, mins, charge int
		var rate float64
		if err := rrows.Scan(&id, &callID, &started, &ended, &did, &userID,
			&billsec, &mins, &rate, &charge, &hc, &src, &dst, &fdt, &fd); err != nil {
			continue
		}
		if fdt == "sip_account" && fd != "" && h.PublicIP != "" {
			fd = fd + "@" + h.PublicIP
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), callID, started, ended, did,
			strconv.FormatInt(userID, 10),
			strconv.Itoa(billsec), strconv.Itoa(mins),
			strconv.FormatFloat(rate, 'f', 4, 64),
			strconv.Itoa(charge),
			hc, src, dst, fd, fdt,
		})
	}
	cw.Flush()
}
