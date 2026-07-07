package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"didstorage/internal/auth"
	"didstorage/internal/domain"
)

// safeReturnTo reads a return_to form field (set by the page that submitted
// this POST so the user lands back where they were, e.g. /dids?status=reserved).
// Defends against open-redirect by requiring the value to be a same-origin
// path: starts with "/" but not "//" (which would let an attacker craft
// "//evil.com" as a protocol-relative URL). Falls back to the supplied
// default when missing or invalid.
func safeReturnTo(r *http.Request, fallback string) string {
	v := strings.TrimSpace(r.PostForm.Get("return_to"))
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return fallback
	}
	return v
}

// didReserve flips an available DID into status='reserved' with a hard-coded
// admin-supplied route. Used for testing and staging — calls hitting that
// DID get the reservation route, no billing, no user/order link.
//
// Two reservation shapes are supported:
//
//   - SIP routing: route_kind ∈ (sip_uri|ip|sip_account), route_target is a
//     free-text URI/IP/account name validated by NormalizeRouteTarget.
//   - Audio playback: route_kind='audio', audio_file_id picks a clip from
//     the library; the DID answers the call, plays the clip once, hangs up.
//     reserved_route_target is filled with the playback target string the
//     dialplan expects ("didstorage/<basename>") so the AGI path stays
//     identical to the SIP routes.
func (h *Handler) didReserve(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	back := safeReturnTo(r, "/dids")
	rk := strings.TrimSpace(r.PostForm.Get("route_kind"))
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if rk == "" {
		h.flashErr(r, "Route kind is required.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	adminID := auth.AdminIDFromSession(h.Session, r)
	var (
		rt              string
		audioIDArg      any // nullable bigint param for the UPDATE
		audioGroupIDArg any // nullable bigint param for the UPDATE
	)

	switch rk {
	case "audio":
		afID := atoi64(r.PostForm.Get("audio_file_id"))
		if afID <= 0 {
			h.flashErr(r, "Pick an audio file from the dropdown.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		var filename, name string
		err := h.DB.QueryRow(r.Context(),
			`SELECT filename, name FROM audio_files WHERE id = $1`, afID).Scan(&filename, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			h.flashErr(r, "That audio file no longer exists. Refresh the page and pick another clip.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if err != nil {
			h.Log.Error("audio reserve lookup", "err", err)
			h.flashErr(r, "Audio lookup failed. Check the server logs for details.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		// Embed the Asterisk-relative playback target so /sipctl/authorize
		// can return it verbatim — no second DB lookup needed at call time.
		// audio.PlaybackPrefix lives in internal/audio but using the literal
		// here avoids an import-cycle worry; it's a one-line constant that
		// changes only if we move the sounds dir.
		rt = "didstorage/" + filename
		audioIDArg = afID
		// Re-stamp display value used in the flash so the operator sees the
		// clip name, not the on-disk basename.
		defer h.flashOK(r, fmt.Sprintf("DID reserved. Audio clip: %s.", name))

	case "audio_group":
		// audio_group reservations resolve to a concrete audio_files row
		// per INVITE inside sipctl.pickAudioGroupMember. Here we just
		// validate the group exists and has at least one member, then
		// store the group id; reserved_route_target stays NULL.
		agID := atoi64(r.PostForm.Get("audio_group_id"))
		if agID <= 0 {
			h.flashErr(r, "Pick an audio group from the dropdown.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		var name string
		var members int
		err := h.DB.QueryRow(r.Context(), `
			SELECT g.name,
			       (SELECT count(*) FROM audio_group_members m WHERE m.group_id = g.id)
			  FROM audio_groups g WHERE g.id = $1`, agID).Scan(&name, &members)
		if errors.Is(err, pgx.ErrNoRows) {
			h.flashErr(r, "That audio group no longer exists. Refresh the page and pick another.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if err != nil {
			h.Log.Error("audio_group reserve lookup", "err", err)
			h.flashErr(r, "Group lookup failed. Check the server logs for details.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		if members == 0 {
			h.flashErr(r, fmt.Sprintf("Group %q is empty. Add audio files to it first.", name))
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		audioGroupIDArg = agID
		// rt stays empty — sipctl materialises it at call time.
		defer h.flashOK(r, fmt.Sprintf("DID reserved. Audio group: %s (%d clip%s, random-no-repeat).",
			name, members, func() string { if members == 1 { return "" }; return "s" }()))

	default:
		rt = domain.NormalizeRouteTarget(rk, r.PostForm.Get("route_target"))
		if rt == "" {
			h.flashErr(r, "Route target is required.")
			http.Redirect(w, r, back, http.StatusFound)
			return
		}
		defer h.flashOK(r, fmt.Sprintf("DID reserved. %s: %s.", rk, rt))
	}

	tag, err := h.DB.Exec(r.Context(), `
		UPDATE dids
		   SET status='reserved',
		       reserved_route_kind=$1::route_kind,
		       reserved_route_target=NULLIF($2,''),
		       reserved_note=$3,
		       reserved_at=now(),
		       reserved_by=$4,
		       reserved_audio_file_id=$5,
		       reserved_audio_group_id=$6
		 WHERE id=$7 AND status='available'`,
		rk, rt, note, adminID, audioIDArg, audioGroupIDArg, id)
	if err != nil {
		h.Log.Error("did reserve", "err", err)
		h.flashErr(r, "Reserve failed. Check the server logs for details.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "DID is not available. Release it first, or cancel its active order.")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	http.Redirect(w, r, back, http.StatusFound)
}

// didRelease clears a reservation, returning the DID to status='available'.
// We snapshot the reservation into did_reservation_history before clearing
// the dids row so compliance/ops keep a permanent audit trail of who
// reserved each DID, why, when, and who released it. Done in one tx so we
// never produce a history row for a release that didn't happen — or vice
// versa.
func (h *Handler) didRelease(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	r.ParseForm()
	back := safeReturnTo(r, "/dids")
	releasedBy := auth.AdminIDFromSession(h.Session, r)

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, "release: "+err.Error())
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	// SELECT FOR UPDATE locks the row so a concurrent release/reserve
	// can't sneak in between archiving and clearing.
	var (
		rkPtr, rtPtr, notePtr *string
		reservedAtPtr         *string // raw timestamptz formatted
		reservedByPtr         *int64
	)
	err = tx.QueryRow(r.Context(), `
		SELECT reserved_route_kind::text, reserved_route_target, reserved_note,
		       to_char(reserved_at,'YYYY-MM-DD"T"HH24:MI:SS.US"+00"'),
		       reserved_by
		  FROM dids
		 WHERE id=$1 AND status='reserved'
		 FOR UPDATE`, id).Scan(&rkPtr, &rtPtr, &notePtr, &reservedAtPtr, &reservedByPtr)
	if errors.Is(err, pgx.ErrNoRows) {
		h.flashErr(r, "DID is not reserved")
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	if err != nil {
		h.flashErr(r, "release: "+err.Error())
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	// Archive into history. reserved_at can be NULL on legacy rows that
	// pre-date that column being populated; in that case we substitute now()
	// so the NOT NULL constraint holds and the row is at least roughly
	// timestamped.
	_, err = tx.Exec(r.Context(), `
		INSERT INTO did_reservation_history
		    (did_id, reserved_route_kind, reserved_route_target, reserved_note,
		     reserved_at, reserved_by, released_by)
		VALUES ($1, $2::route_kind, $3, $4,
		        COALESCE($5::timestamptz, now()), $6, $7)`,
		id, rkPtr, rtPtr, notePtr, reservedAtPtr, reservedByPtr, releasedBy)
	if err != nil {
		h.flashErr(r, "release archive: "+err.Error())
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	// reserved_audio_file_id / reserved_audio_group_id are cleared alongside
	// the other reserved_* columns — without this an audio reservation's
	// FK survives release and the audio-library "in use" counter never
	// drops back to zero.
	_, err = tx.Exec(r.Context(), `
		UPDATE dids
		   SET status='available',
		       reserved_route_kind=NULL, reserved_route_target=NULL,
		       reserved_note=NULL, reserved_at=NULL, reserved_by=NULL,
		       reserved_audio_file_id=NULL,
		       reserved_audio_group_id=NULL
		 WHERE id=$1`, id)
	if err != nil {
		h.flashErr(r, "release: "+err.Error())
		http.Redirect(w, r, back, http.StatusFound)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "release commit: "+err.Error())
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	h.flashOK(r, "DID released back to available")
	http.Redirect(w, r, back, http.StatusFound)
}

// didCDRs lists all CDRs for one DID, regardless of which order/reservation
// the call hit. Each row shows the route at call time (cdrs.routed_*) plus
// the user/reseller/order context.
func (h *Handler) didCDRs(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var d struct {
		ID                              int64
		E164, Country, Type, Supplier   string
		Status                          string
		ReservedKind, ReservedTarget    string
		ReservedNote                    string
	}
	err := h.DB.QueryRow(r.Context(), `
		SELECT d.id, d.e164, d.country_iso, d.did_type::text, s.name, d.status,
		       COALESCE(d.reserved_route_kind::text,''), COALESCE(d.reserved_route_target,''),
		       COALESCE(d.reserved_note,'')
		  FROM dids d JOIN suppliers s ON s.id=d.supplier_id
		 WHERE d.id=$1`, id,
	).Scan(&d.ID, &d.E164, &d.Country, &d.Type, &d.Supplier, &d.Status,
		&d.ReservedKind, &d.ReservedTarget, &d.ReservedNote)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}

	allowedSorts := map[string]string{
		"started": "c.started_at",
		"billsec": "c.billsec",
		"charge":  "c.charge_cents",
	}
	pg := readPagination(r, fmt.Sprintf("/dids/%d/cdrs", id), allowedSorts, "started")

	type row struct {
		ID                          int64
		CallID                      string
		Started                     string
		Billsec, ChargedMinutes     int
		ChargeDollars               float64
		HangupCause                 string
		SrcURI, DstURI              string
		RoutedKind, RoutedTarget    string
		OrderID                     int64
		UserID                      int64
		UserRef                     string
		Reseller                    string
		ResellerID                  int64
		State                       string
	}

	var total int
	h.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM cdrs WHERE did_id=$1`, id).Scan(&total)
	pg.Total = total

	args := []any{id, pg.Limit(), pg.Offset()}
	sql := `
		SELECT c.id, c.call_id, to_char(c.started_at,'YYYY-MM-DD HH24:MI:SS'),
		       c.billsec, c.charged_minutes, c.charge_cents, COALESCE(c.hangup_cause,''),
		       COALESCE(c.src_uri,''), COALESCE(c.dst_uri,''),
		       COALESCE(c.routed_kind::text,''), COALESCE(c.routed_target,''),
		       COALESCE(c.order_id, 0),
		       COALESCE(c.user_id, 0),
		       COALESCE((SELECT COALESCE(u.external_id, u.label, u.contact_email, '')
		                   FROM users u WHERE u.id = c.user_id), ''),
		       COALESCE((SELECT re.name FROM resellers re
		                  JOIN users u ON u.id = c.user_id WHERE re.id = u.reseller_id), ''),
		       COALESCE((SELECT u.reseller_id FROM users u WHERE u.id = c.user_id), 0)
		  FROM cdrs c
		 WHERE c.did_id = $1` +
		orderByClause(allowedSorts, pg) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", 2, 3)

	rows, err := h.DB.Query(r.Context(), sql, args...)
	if err != nil {
		h.Log.Error("did cdrs query", "err", err)
		http.Error(w, "internal", 500)
		return
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var x row
		var charge int
		if err := rows.Scan(&x.ID, &x.CallID, &x.Started, &x.Billsec, &x.ChargedMinutes,
			&charge, &x.HangupCause, &x.SrcURI, &x.DstURI,
			&x.RoutedKind, &x.RoutedTarget, &x.OrderID, &x.UserID, &x.UserRef,
			&x.Reseller, &x.ResellerID); err == nil {
			x.ChargeDollars = float64(charge) / 100
			x.State = domain.CallState(x.Billsec, x.HangupCause)
			out = append(out, x)
		}
	}

	// Reservation history — every past reserve→release cycle archived in
	// did_reservation_history (added 0012). Empty for DIDs that never were
	// reserved; otherwise newest-first.
	type resvRow struct {
		RouteKind, RouteTarget, Note string
		ReservedAt, ReleasedAt       string
		ReservedByEmail              string
		ReleasedByEmail              string
	}
	var resvs []resvRow
	rrows, _ := h.DB.Query(r.Context(), `
		SELECT COALESCE(rh.reserved_route_kind::text,''),
		       COALESCE(rh.reserved_route_target,''),
		       COALESCE(rh.reserved_note,''),
		       to_char(rh.reserved_at,'YYYY-MM-DD HH24:MI'),
		       to_char(rh.released_at,'YYYY-MM-DD HH24:MI'),
		       COALESCE(ar.email,''),
		       COALESCE(al.email,'')
		  FROM did_reservation_history rh
		  LEFT JOIN admins ar ON ar.id = rh.reserved_by
		  LEFT JOIN admins al ON al.id = rh.released_by
		 WHERE rh.did_id=$1
		 ORDER BY rh.released_at DESC
		 LIMIT 50`, id)
	for rrows.Next() {
		var x resvRow
		if err := rrows.Scan(&x.RouteKind, &x.RouteTarget, &x.Note,
			&x.ReservedAt, &x.ReleasedAt, &x.ReservedByEmail, &x.ReleasedByEmail); err == nil {
			resvs = append(resvs, x)
		}
	}
	rrows.Close()

	ok, em := h.popFlashes(r)
	h.render(w, "did_cdrs", map[string]any{
		"Title":           "DID · " + d.E164 + " · CDRs",
		"Section":         "dids",
		"FlashOK":         ok,
		"FlashErr":        em,
		"DID":             d,
		"CDRs":            out,
		"Pg":              pg,
		"ReservationLog":  resvs,
	})
}
