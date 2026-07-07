package web

import (
	"net/http"
	"strings"
)

// globalSearch is the target of the top-bar search box. It runs a small set
// of queries across users, DIDs, orders, and CDR call-IDs, and renders a
// page with whichever resource types matched. Empty q → just renders the
// search prompt.
//
// Why not a fancy fts index? We don't have the volume to need it; ILIKE on
// the small set of "user-typed" fields below is plenty fast and avoids the
// operational overhead of pg_trgm or tsvector maintenance.
func (h *Handler) globalSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	results := map[string]any{
		"Title":   "Search",
		"Section": "search",
		"GlobalQ": q,
		"Q":       q,
	}
	if q == "" {
		h.render(w, "search", results)
		return
	}

	pat := "%" + q + "%"

	type userHit struct {
		ID         int64
		ExternalID string
		Label      string
		Status     string
	}
	var users []userHit
	urows, _ := h.DB.Query(r.Context(), `
		SELECT id, COALESCE(external_id,''), COALESCE(label, COALESCE(contact_email,'')), status
		  FROM users
		 WHERE external_id ILIKE $1 OR label ILIKE $1 OR contact_email ILIKE $1
		    OR cast(id as text) = $2
		 ORDER BY id LIMIT 25`, pat, q)
	for urows.Next() {
		var u userHit
		urows.Scan(&u.ID, &u.ExternalID, &u.Label, &u.Status)
		users = append(users, u)
	}
	urows.Close()
	results["Users"] = users

	type didHit struct {
		ID       int64
		E164     string
		Status   string
		Supplier string
	}
	var dids []didHit
	drows, _ := h.DB.Query(r.Context(), `
		SELECT d.id, d.e164, d.status, s.name
		  FROM dids d JOIN suppliers s ON s.id=d.supplier_id
		 WHERE d.e164 ILIKE $1 ORDER BY d.e164 LIMIT 25`, pat)
	for drows.Next() {
		var d didHit
		drows.Scan(&d.ID, &d.E164, &d.Status, &d.Supplier)
		dids = append(dids, d)
	}
	drows.Close()
	results["DIDs"] = dids

	type orderHit struct {
		ID     int64
		E164   string
		User   string
		Status string
	}
	var orders []orderHit
	orows, _ := h.DB.Query(r.Context(), `
		SELECT o.id, d.e164, COALESCE(u.external_id,u.label,u.contact_email,''), o.status::text
		  FROM orders o
		  JOIN dids  d ON d.id=o.did_id
		  JOIN users u ON u.id=o.user_id
		 WHERE cast(o.id as text)=$1 OR d.e164 ILIKE $2
		 ORDER BY o.id DESC LIMIT 25`, q, pat)
	for orows.Next() {
		var o orderHit
		orows.Scan(&o.ID, &o.E164, &o.User, &o.Status)
		orders = append(orders, o)
	}
	orows.Close()
	results["Orders"] = orders

	// CDRs: match by call_id prefix (sanitized form is what we store).
	type cdrHit struct {
		CallID  string
		DID     string
		User    string
		Started string
		State   string
	}
	var cdrs []cdrHit
	crows, _ := h.DB.Query(r.Context(), `
		SELECT c.call_id, d.e164, COALESCE(u.external_id,u.label,u.contact_email,''),
		       to_char(c.started_at,'YYYY-MM-DD HH24:MI'),
		       c.billsec, COALESCE(c.hangup_cause,'')
		  FROM cdrs c
		  JOIN orders o ON o.id=c.order_id
		  JOIN dids   d ON d.id=o.did_id
		  JOIN users  u ON u.id=c.user_id
		 WHERE c.call_id ILIKE $1
		 ORDER BY c.started_at DESC LIMIT 25`, pat)
	for crows.Next() {
		var x cdrHit
		var billsec int
		var hc string
		crows.Scan(&x.CallID, &x.DID, &x.User, &x.Started, &billsec, &hc)
		x.State = "answered"
		if billsec == 0 {
			x.State = "failed"
		}
		cdrs = append(cdrs, x)
	}
	crows.Close()
	results["CDRs"] = cdrs

	h.render(w, "search", results)
}
