package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CSV exports for compliance / accounting use. Each handler streams a CSV
// directly so we don't buffer everything in memory.
//
//	GET /users/{id}/export/cdrs.csv?from=YYYY-MM-DD&to=YYYY-MM-DD
//	GET /users/{id}/export/ledger.csv?from=...&to=...
//	GET /users/{id}/export/blocks.csv
//	GET /orders/{id}/export/cdrs.csv?from=...&to=...
//
// Date range is inclusive of `from` and exclusive of `to`. Both optional.

func dateRange(r *http.Request) (from, to time.Time, hasFrom, hasTo bool) {
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

func csvHeader(w http.ResponseWriter, filename string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return csv.NewWriter(w)
}

// globalExportCDRs is the CSV export targeted by the /cdrs page's "CSV"
// button. It accepts every filter the /cdrs page does (reseller_id, user_id,
// order_id, external_id, did, q, state, from, to) and emits the same column
// layout as the per-user / per-order exports.
func (h *Handler) globalExportCDRs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var args []any
	where := " WHERE 1=1"
	add := func(clause string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND %s $%d", clause, len(args))
	}
	if v := q.Get("reseller_id"); v != "" {
		if v == "0" {
			where += " AND u.reseller_id IS NULL"
		} else if n, _ := strconv.ParseInt(v, 10, 64); n > 0 {
			add("u.reseller_id =", n)
		}
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
		where += " AND c.billsec > 0"
	case "failed":
		where += " AND c.billsec = 0"
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		add("(c.call_id ILIKE", "%"+v+"%")
		// Reuse the same arg for src_uri / dst_uri via the next two clauses.
		// We can't $N twice in the simple add() helper, so emit it inline.
		where += " OR c.src_uri ILIKE $" + strconv.Itoa(len(args)) +
			" OR c.dst_uri ILIKE $" + strconv.Itoa(len(args)) + ")"
	}

	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       to_char(c.ended_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(d.e164, ''), COALESCE(c.user_id, 0), COALESCE(c.order_id, 0),
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
		  LEFT JOIN users  u ON u.id = c.user_id` + where +
		` ORDER BY c.started_at DESC`
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	cw := csvHeader(w, "cdrs.csv")
	cw.Write([]string{"cdr_id", "call_id", "started_at", "ended_at", "did_e164",
		"user_id", "order_id", "billsec", "charged_minutes",
		"rate_cents_per_min", "charge_cents", "hangup_cause",
		"src", "dst", "final_dst", "final_dst_type"})
	for rows.Next() {
		var id, userID, orderID int64
		var callID, started, ended, did, hc, src, dst, fdt, fd string
		var billsec, mins, charge int
		var rate float64
		if err := rows.Scan(&id, &callID, &started, &ended, &did, &userID, &orderID,
			&billsec, &mins, &rate, &charge, &hc, &src, &dst, &fdt, &fd); err != nil {
			continue
		}
		if fdt == "sip_account" && fd != "" && h.PublicIP != "" {
			fd = fd + "@" + h.PublicIP
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), callID, started, ended, did,
			strconv.FormatInt(userID, 10), strconv.FormatInt(orderID, 10),
			strconv.Itoa(billsec), strconv.Itoa(mins),
			strconv.FormatFloat(rate, 'f', 4, 64),
			strconv.Itoa(charge),
			hc, src, dst, fd, fdt,
		})
	}
	cw.Flush()
}

func (h *Handler) userExportCDRs(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	from, to, hasFrom, hasTo := dateRange(r)
	// Use cdrs.routed_kind / cdrs.routed_target snapshots when available
	// (post-0005 migration). Fall back to whatever the order has now for
	// older rows.
	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       to_char(c.ended_at,'YYYY-MM-DD HH24:MI:SS'),
		       COALESCE(d.e164, '')          AS did_e164,
		       COALESCE(c.order_id, 0),
		       c.billsec, c.charged_minutes,
		       c.rate_cents_per_min, c.charge_cents,
		       COALESCE(c.hangup_cause,''),
		       COALESCE(c.src_uri,''),
		       COALESCE(c.dst_uri,''),
		       COALESCE(c.routed_kind::text,
		                COALESCE(o.route_kind::text, '')) AS final_dst_type,
		       COALESCE(c.routed_target,
		                COALESCE(o.route_target, ''))     AS final_dst
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
	cw := csvHeader(w, fmt.Sprintf("cdrs-user-%d.csv", uid))
	// Final two cols are now src/dst/final_dst/final_dst_type. final_dst is
	// the actual route target (what the inbound got forwarded to). For
	// route_kind=sip_account we expand 'username' to 'username@<our_ip>' so
	// the row alone identifies the destination peer.
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

func (h *Handler) userExportLedger(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	from, to, hasFrom, hasTo := dateRange(r)
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
	cw := csvHeader(w, fmt.Sprintf("ledger-user-%d.csv", uid))
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

func (h *Handler) userExportBlocks(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, to_char(created_at,'YYYY-MM-DD HH24:MI:SS'),
		       action::text, reason, kyc_bundle_id,
		       COALESCE((SELECT a.email FROM admins a WHERE a.id = blocked_by), '')
		  FROM user_block_log
		 WHERE user_id=$1 ORDER BY id`, uid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	cw := csvHeader(w, fmt.Sprintf("blocks-user-%d.csv", uid))
	cw.Write([]string{"id", "created_at", "action", "reason", "kyc_bundle_id", "blocked_by"})
	for rows.Next() {
		var id int64
		var bundle *int64
		var created, action, reason, by string
		if err := rows.Scan(&id, &created, &action, &reason, &bundle, &by); err != nil {
			continue
		}
		bid := ""
		if bundle != nil {
			bid = strconv.FormatInt(*bundle, 10)
		}
		cw.Write([]string{
			strconv.FormatInt(id, 10), created, action, reason, bid, by,
		})
	}
	cw.Flush()
}

func (h *Handler) orderExportCDRs(w http.ResponseWriter, r *http.Request) {
	oid := pathID(r, "id")
	from, to, hasFrom, hasTo := dateRange(r)
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
	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	cw := csvHeader(w, fmt.Sprintf("cdrs-order-%d.csv", oid))
	cw.Write([]string{"cdr_id", "call_id", "started_at", "ended_at", "did_e164",
		"user_id", "billsec", "charged_minutes", "rate_cents_per_min", "charge_cents",
		"hangup_cause", "src", "dst", "final_dst", "final_dst_type"})
	for rows.Next() {
		var id, userID int64
		var callID, started, ended, did, hc, src, dst, fdt, fd string
		var billsec, mins, charge int
		var rate float64
		if err := rows.Scan(&id, &callID, &started, &ended, &did, &userID,
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
