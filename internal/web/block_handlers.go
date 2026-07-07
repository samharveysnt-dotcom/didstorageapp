package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"didstorage/internal/auth"
)

// userBlock cascades a user-level block down to all of the user's active
// orders, which go to status='quarantined'. Their pre-block route is saved to
// pre_quarantine_route_kind / pre_quarantine_route_target so unblock can
// restore them. An entry is added to user_block_log for audit.
//
// During quarantine, sipctl Authorize replies deny+SIPCode=480 (Temporarily
// Unavailable) instead of failing as unknown_did.
func (h *Handler) userBlock(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	r.ParseForm()
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		h.flashErr(r, "block reason required (it's recorded in the compliance log)")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	var bundlePtr *int64
	if v := atoi64(r.PostForm.Get("kyc_bundle_id")); v > 0 {
		bundlePtr = &v
	}
	adminID := auth.AdminIDFromSession(h.Session, r)

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	tag, err := tx.Exec(r.Context(), `
		UPDATE users SET status='inactive' WHERE id=$1 AND status='active'`, uid)
	if err != nil {
		h.flashErr(r, "block user: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "user is not currently active — nothing to do")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	// Cascade: stash each active order's route, flip to quarantined.
	if _, err := tx.Exec(r.Context(), `
		UPDATE orders
		   SET pre_quarantine_route_kind   = route_kind,
		       pre_quarantine_route_target = route_target,
		       status                      = 'quarantined'
		 WHERE user_id=$1 AND status='active'`, uid); err != nil {
		h.flashErr(r, "cascade quarantine: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO user_block_log (user_id, action, reason, kyc_bundle_id, blocked_by)
		VALUES ($1, 'block', $2, $3, $4)`,
		uid, reason, bundlePtr, nullableAdmin(adminID)); err != nil {
		h.flashErr(r, "audit: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
	} else {
		h.flashOK(r, "User blocked — orders quarantined, calls reply 480")
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
}

// userUnblock reverses a block: user.status → active, each quarantined order's
// route is restored from pre_quarantine_*, status → active. Audit log entry.
func (h *Handler) userUnblock(w http.ResponseWriter, r *http.Request) {
	uid := pathID(r, "id")
	r.ParseForm()
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if reason == "" {
		reason = "unblocked"
	}
	adminID := auth.AdminIDFromSession(h.Session, r)

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	tag, err := tx.Exec(r.Context(), `
		UPDATE users SET status='active' WHERE id=$1 AND status='inactive'`, uid)
	if err != nil {
		h.flashErr(r, "unblock user: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}
	if tag.RowsAffected() == 0 {
		h.flashErr(r, "user is not currently blocked")
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	// Restore: any quarantined order goes back to active and its old route
	// is re-applied. We blank the pre_* columns afterwards so a future block
	// captures fresh state.
	if _, err := tx.Exec(r.Context(), `
		UPDATE orders
		   SET route_kind   = COALESCE(pre_quarantine_route_kind, route_kind),
		       route_target = COALESCE(pre_quarantine_route_target, route_target),
		       pre_quarantine_route_kind   = NULL,
		       pre_quarantine_route_target = NULL,
		       status = 'active'
		 WHERE user_id=$1 AND status='quarantined'`, uid); err != nil {
		h.flashErr(r, "restore: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO user_block_log (user_id, action, reason, blocked_by)
		VALUES ($1, 'unblock', $2, $3)`,
		uid, reason, nullableAdmin(adminID)); err != nil {
		h.flashErr(r, "audit: "+err.Error())
		http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, "commit: "+err.Error())
	} else {
		h.flashOK(r, "User unblocked — orders restored to their previous routes")
	}
	http.Redirect(w, r, fmt.Sprintf("/users/%d", uid), http.StatusFound)
}

// nullableAdmin lets us pass NULL when no admin id is in session (e.g., a
// future cron-driven block). adminID = 0 → SQL NULL.
func nullableAdmin(adminID int64) any {
	if adminID == 0 {
		return sql.NullInt64{}
	}
	return adminID
}

// loadBlockHistory is used by the user detail template to show the audit
// trail. Returns most recent first.
func (h *Handler) loadBlockHistory(ctx context.Context, userID int64, limit int) []blockLogRow {
	var out []blockLogRow
	rows, err := h.DB.Query(ctx, `
		SELECT to_char(created_at,'YYYY-MM-DD HH24:MI'), action::text, reason,
		       kyc_bundle_id,
		       COALESCE((SELECT a.email FROM admins a WHERE a.id = blocked_by), '')
		  FROM user_block_log
		 WHERE user_id=$1
		 ORDER BY id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x blockLogRow
		var bundle *int64
		if err := rows.Scan(&x.Created, &x.Action, &x.Reason, &bundle, &x.By); err == nil {
			if bundle != nil {
				x.KycBundleID = *bundle
			}
			out = append(out, x)
		}
	}
	return out
}

type blockLogRow struct {
	Created     string
	Action      string
	Reason      string
	KycBundleID int64
	By          string
}

// blockHistoryFor is exposed so userDetail can include the rows.
func (h *Handler) blockHistoryFor(ctx context.Context, userID int64) []blockLogRow {
	return h.loadBlockHistory(ctx, userID, 50)
}

// quietPgxNoRows turns "no rows" into a benign empty result for places that
// don't care to distinguish absent vs error. Used in a couple of helpers.
func quietPgxNoRows(err error) error {
	if err == nil {
		return nil
	}
	if err == pgx.ErrNoRows {
		return nil
	}
	return err
}
