package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// orderDetail renders a single order's full context with six tabs:
//
//	Overview  — billing math, rate-card snapshot, recent activity summary
//	Peers     — SIP accounts owned by the order's user (the route_target
//	            for sip_account routes will point at one of these)
//	CDRs      — every call ever routed through this order
//	KYC       — bundle currently attached + any sibling bundles on the user
//	Ledger    — every ledger entry referencing this order (NRC + call
//	            charges + MRC + channel fee)
//	Compliance — user_block_log rows scoped to this order, including the
//	             live_hangup / live_warn / live_redirect events written by
//	             the /live page actions
//
// Mirrors user_detail.go's tab pattern but everything joins on order_id so
// the view is per-DID-rental. Cancelled/expired orders still render — the
// caller arrived here via /orders?view=archive and an audit trail is the
// whole point of preserving them.
func (h *Handler) orderDetail(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")

	// ----- Order header + rate-card snapshot -----
	type orderHdr struct {
		ID, UserID, DIDID, SupplierID, RateCardID, KycBundleID int64
		E164, Country, Type, Supplier                          string
		UserRef, UserStatus                                    string
		ResellerID                                             int64
		ResellerName                                           string
		Status, RouteKind, RouteTarget                         string
		// AudioGroupID is set when RouteKind=audio_group; nil/0 otherwise.
		// Drives the route-edit form's pre-selection of the existing group.
		AudioGroupID                                           int64
		ChannelCount, AnniversaryDay                           int
		AssignedAt, NextBillingAt, EndedAt                     string
		KycBundleStatus                                        string
		// rate-card snapshot
		NRCDollars, MRCDollars, ChannelMonthlyDollars   float64
		PerMinuteCents                                  float64
		// Customer-side billing increment ("60/60" by default).
		BillMinSeconds, BillIncrementSeconds            int
		SupNRCDollars, SupMRCDollars                    float64
		SupPerMinuteCents                               float64
		// Supplier-side billing increment — what the supplier charges us.
		SupBillMinSeconds, SupBillIncrementSeconds      int
	}
	var hd orderHdr
	var nrc, mrc, chMo, supNRC, supMRC int
	var perMin, supPerMin float64
	err := h.DB.QueryRow(r.Context(), `
		SELECT o.id, o.user_id, o.did_id, d.supplier_id, o.rate_card_id, COALESCE(o.kyc_bundle_id, 0),
		       d.e164, d.country_iso, d.did_type::text, s.name,
		       COALESCE(u.external_id, u.label, u.contact_email, ''), u.status,
		       COALESCE(u.reseller_id, 0), COALESCE(re.name, ''),
		       o.status::text, o.route_kind::text, COALESCE(o.route_target,''),
		       COALESCE(o.audio_group_id, 0),
		       o.channel_count, o.anniversary_day,
		       to_char(o.assigned_at, 'YYYY-MM-DD HH24:MI'),
		       to_char(o.next_billing_at, 'YYYY-MM-DD'),
		       COALESCE(to_char(o.ended_at, 'YYYY-MM-DD HH24:MI'), ''),
		       COALESCE((SELECT b.status::text FROM kyc_bundles b WHERE b.id = o.kyc_bundle_id), ''),
		       rc.nrc_cents, rc.mrc_cents, rc.channel_monthly_cents, rc.per_minute_cents,
		       rc.bill_min_seconds, rc.bill_increment_seconds,
		       rc.supplier_nrc_cents, rc.supplier_mrc_cents, rc.supplier_per_minute_cents,
		       rc.supplier_bill_min_seconds, rc.supplier_bill_increment_seconds
		  FROM orders o
		  JOIN dids        d  ON d.id  = o.did_id
		  JOIN suppliers   s  ON s.id  = d.supplier_id
		  JOIN users       u  ON u.id  = o.user_id
		  LEFT JOIN resellers  re ON re.id = u.reseller_id
		  JOIN rate_cards  rc ON rc.id = o.rate_card_id
		 WHERE o.id = $1`, id).Scan(
		&hd.ID, &hd.UserID, &hd.DIDID, &hd.SupplierID, &hd.RateCardID, &hd.KycBundleID,
		&hd.E164, &hd.Country, &hd.Type, &hd.Supplier,
		&hd.UserRef, &hd.UserStatus, &hd.ResellerID, &hd.ResellerName,
		&hd.Status, &hd.RouteKind, &hd.RouteTarget,
		&hd.AudioGroupID,
		&hd.ChannelCount, &hd.AnniversaryDay,
		&hd.AssignedAt, &hd.NextBillingAt, &hd.EndedAt, &hd.KycBundleStatus,
		&nrc, &mrc, &chMo, &perMin,
		&hd.BillMinSeconds, &hd.BillIncrementSeconds,
		&supNRC, &supMRC, &supPerMin,
		&hd.SupBillMinSeconds, &hd.SupBillIncrementSeconds,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("order detail header", "err", err, "order_id", id)
		http.Error(w, "internal", 500)
		return
	}
	hd.NRCDollars = float64(nrc) / 100
	hd.MRCDollars = float64(mrc) / 100
	hd.ChannelMonthlyDollars = float64(chMo) / 100
	hd.PerMinuteCents = perMin
	hd.SupNRCDollars = float64(supNRC) / 100
	hd.SupMRCDollars = float64(supMRC) / 100
	hd.SupPerMinuteCents = supPerMin

	// ----- Revenue / supplier-cost totals (same shape as /orders list) -----
	var rev30, sup30, revLife, supLife int64
	_ = h.DB.QueryRow(r.Context(), `
		SELECT
		  COALESCE((SELECT SUM(charge_cents) FROM cdrs
		             WHERE order_id = $1 AND started_at > now() - interval '30 days'), 0)
		  + COALESCE((SELECT SUM(mrc_charged_cents + channel_charged_cents) FROM billing_runs
		               WHERE order_id = $1 AND ran_at > now() - interval '30 days'), 0)
		  + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		               WHERE ref_table='orders' AND ref_id=$1 AND kind='nrc'
		                 AND created_at > now() - interval '30 days'), 0),
		  COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		             WHERE order_id = $1 AND supplier_charge_cents IS NOT NULL
		               AND started_at > now() - interval '30 days'), 0)
		  + $2 * (SELECT count(*) FROM billing_runs
		            WHERE order_id = $1 AND ran_at > now() - interval '30 days')
		  + CASE WHEN (SELECT assigned_at FROM orders WHERE id = $1) > now() - interval '30 days'
		         THEN $3 ELSE 0 END,
		  COALESCE((SELECT SUM(charge_cents) FROM cdrs WHERE order_id = $1), 0)
		  + COALESCE((SELECT SUM(mrc_charged_cents + channel_charged_cents) FROM billing_runs WHERE order_id = $1), 0)
		  + COALESCE((SELECT -SUM(delta_cents) FROM balance_ledger
		               WHERE ref_table='orders' AND ref_id=$1 AND kind='nrc'), 0),
		  COALESCE((SELECT SUM(supplier_charge_cents) FROM cdrs
		             WHERE order_id = $1 AND supplier_charge_cents IS NOT NULL), 0)
		  + $2 * (SELECT count(*) FROM billing_runs WHERE order_id = $1)
		  + $3
		`, id, supMRC, supNRC).Scan(&rev30, &sup30, &revLife, &supLife)

	// ----- Peers — SIP accounts on this order's user. Mark the one we route to. -----
	type peerRow struct {
		ID                int64
		Username, Realm   string
		Created           string
		IsRouteTarget     bool
	}
	var peers []peerRow
	prows, _ := h.DB.Query(r.Context(), `
		SELECT id, username, realm, to_char(created_at,'YYYY-MM-DD HH24:MI')
		  FROM sip_accounts WHERE user_id = $1 ORDER BY id`, hd.UserID)
	for prows.Next() {
		var p peerRow
		if err := prows.Scan(&p.ID, &p.Username, &p.Realm, &p.Created); err == nil {
			if hd.RouteKind == "sip_account" && p.Username == hd.RouteTarget {
				p.IsRouteTarget = true
			}
			peers = append(peers, p)
		}
	}
	prows.Close()

	// ----- CDRs (last 100 for this order) -----
	type cdrRow struct {
		ID                                   int64
		CallID, Started, State, SrcURI       string
		Billsec, ChargedMinutes              int
		ChargeDollars, SupplierChargeDollars float64
		HangupCause                          string
		RoutedKind, RoutedTarget             string
		AdminAction, AdminActionReason       string
	}
	var cdrs []cdrRow
	crows, _ := h.DB.Query(r.Context(), `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       c.billsec, c.charged_minutes, c.charge_cents,
		       COALESCE(c.supplier_charge_cents, 0),
		       COALESCE(c.hangup_cause,''), COALESCE(c.src_uri,''),
		       COALESCE(c.routed_kind::text,''), COALESCE(c.routed_target,''),
		       COALESCE(c.admin_action::text, ''), COALESCE(c.admin_action_reason, '')
		  FROM cdrs c
		 WHERE c.order_id = $1
		 ORDER BY c.started_at DESC
		 LIMIT 100`, id)
	for crows.Next() {
		var x cdrRow
		var chargeCents, supCents int64
		var billsec, minutes int
		if err := crows.Scan(&x.ID, &x.CallID, &x.Started, &billsec, &minutes, &chargeCents, &supCents,
			&x.HangupCause, &x.SrcURI, &x.RoutedKind, &x.RoutedTarget,
			&x.AdminAction, &x.AdminActionReason); err == nil {
			x.Billsec = billsec
			x.ChargedMinutes = minutes
			x.ChargeDollars = float64(chargeCents) / 100
			x.SupplierChargeDollars = float64(supCents) / 100
			x.State = "answered"
			if billsec == 0 {
				x.State = "failed"
			}
			cdrs = append(cdrs, x)
		}
	}
	crows.Close()

	// ----- KYC bundles: currently-attached + all siblings on the user -----
	type kycRow struct {
		ID                              int64
		Type, Status                    string
		DocCount                        int
		Created, Approved               string
		IsAttached                      bool
	}
	var kycs []kycRow
	krows, _ := h.DB.Query(r.Context(), `
		SELECT b.id, b.type::text, b.status::text,
		       (SELECT count(*) FROM kyc_documents WHERE bundle_id = b.id),
		       to_char(b.created_at, 'YYYY-MM-DD'),
		       COALESCE(to_char(b.approved_at, 'YYYY-MM-DD'), '')
		  FROM kyc_bundles b
		 WHERE b.user_id = $1
		 ORDER BY b.id DESC`, hd.UserID)
	for krows.Next() {
		var k kycRow
		if err := krows.Scan(&k.ID, &k.Type, &k.Status, &k.DocCount, &k.Created, &k.Approved); err == nil {
			if k.ID == hd.KycBundleID {
				k.IsAttached = true
			}
			kycs = append(kycs, k)
		}
	}
	krows.Close()

	// ----- Ledger entries that reference this order -----
	// Three reference paths to scope to one order:
	//   ref_table='orders'        ref_id = order_id           (NRC, manual_adj on order)
	//   ref_table='cdrs'          → JOIN cdrs.order_id
	//   ref_table='billing_runs'  → JOIN billing_runs.order_id
	// UNION ALL keeps them as distinct rows; ORDER BY brings everything in
	// strict time order.
	type ledgerRow struct {
		Created, Kind  string
		Delta          int64
		BalanceAfter   int64
		RefDID         string
	}
	var ledger []ledgerRow
	lrows, _ := h.DB.Query(r.Context(), `
		WITH scoped AS (
		  SELECT bl.id, bl.created_at, bl.kind, bl.delta_cents, bl.balance_after
		    FROM balance_ledger bl
		   WHERE bl.ref_table = 'orders' AND bl.ref_id = $1
		  UNION ALL
		  SELECT bl.id, bl.created_at, bl.kind, bl.delta_cents, bl.balance_after
		    FROM balance_ledger bl
		    JOIN cdrs c ON c.id = bl.ref_id
		   WHERE bl.ref_table = 'cdrs' AND c.order_id = $1
		  UNION ALL
		  SELECT bl.id, bl.created_at, bl.kind, bl.delta_cents, bl.balance_after
		    FROM balance_ledger bl
		    JOIN billing_runs br ON br.id = bl.ref_id
		   WHERE bl.ref_table = 'billing_runs' AND br.order_id = $1
		)
		SELECT to_char(created_at,'YYYY-MM-DD HH24:MI:SS'), kind, delta_cents, balance_after
		  FROM scoped
		 ORDER BY created_at DESC
		 LIMIT 200`, id)
	for lrows.Next() {
		var x ledgerRow
		if err := lrows.Scan(&x.Created, &x.Kind, &x.Delta, &x.BalanceAfter); err == nil {
			ledger = append(ledger, x)
		}
	}
	lrows.Close()

	// ----- Compliance log: events scoped to this order OR user-wide blocks  -----
	type complianceRow struct {
		Created, Action, Reason, Details string
		ByEmail                          string
		Scope                            string // "order" | "user"
	}
	var compliance []complianceRow
	mrows, _ := h.DB.Query(r.Context(), `
		SELECT to_char(bl.created_at,'YYYY-MM-DD HH24:MI:SS'),
		       bl.action::text, bl.reason, COALESCE(bl.details,''),
		       COALESCE(ad.email,''),
		       CASE WHEN bl.order_id IS NOT NULL THEN 'order' ELSE 'user' END
		  FROM user_block_log bl
		  LEFT JOIN admins ad ON ad.id = bl.blocked_by
		 WHERE bl.order_id = $1 OR (bl.user_id = $2 AND bl.order_id IS NULL)
		 ORDER BY bl.created_at DESC
		 LIMIT 200`, id, hd.UserID)
	for mrows.Next() {
		var x complianceRow
		if err := mrows.Scan(&x.Created, &x.Action, &x.Reason, &x.Details, &x.ByEmail, &x.Scope); err == nil {
			compliance = append(compliance, x)
		}
	}
	mrows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "order_detail", map[string]any{
		"Title":               "Order #" + chi.URLParam(r, "id"),
		"Section":             "orders",
		"FlashOK":             ok,
		"FlashErr":            em,
		"Order":               hd,
		"Revenue30dDollars":   float64(rev30) / 100,
		"Profit30dDollars":    float64(rev30-sup30) / 100,
		"RevenueLifeDollars":  float64(revLife) / 100,
		"ProfitLifeDollars":   float64(revLife-supLife) / 100,
		"Peers":               peers,
		"CDRs":                cdrs,
		"KYCBundles":          kycs,
		"Ledger":              ledger,
		"Compliance":          compliance,
	})
}

