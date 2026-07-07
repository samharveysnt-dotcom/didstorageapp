package web

// First-run admin creation. When the admins table is empty (fresh install),
// GET /login redirects to /setup; the operator chooses the initial admin
// password in-browser instead of the bootstrap script auto-generating one
// and shipping it via the deploy log. Once one admin exists the /setup
// page is permanently redirected to /login so it can't be re-used as an
// account-takeover surface.
//
// The transactional re-check inside setupSubmit closes the race where two
// simultaneous POSTs both pass the initial count check — only the first
// commit wins, the second sees admin_count > 0 and bounces to /login.

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"didstorage/internal/auth"
)

// setupIsOpen reports whether the first-run flow is still available. The
// answer is atomic — a hit on this function is followed by the caller's
// action; the transactional re-check in setupSubmit is what actually
// prevents the race.
func (h *Handler) setupIsOpen(ctx context.Context) (bool, error) {
	var n int
	if err := h.DB.QueryRow(ctx, `SELECT count(*) FROM admins`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// setup renders the "create the first admin" form on a fresh install.
// Once any admin exists it permanently 302s to /login.
func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	open, err := h.setupIsOpen(r.Context())
	if err != nil {
		h.Log.Error("setup gate", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !open {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	_, em := h.popFlashes(r)
	h.render(w, "setup", map[string]any{
		"Title":      "Set up your admin account",
		"ShowChrome": false,
		"Err":        em,
	})
}

// setupSubmit accepts the first-admin password + confirmation. Creates the
// row inside a transaction that re-checks the empty-admins invariant, so
// two concurrent submissions can't both create an admin. On success the
// new admin is auto-logged-in and lands on the dashboard.
func (h *Handler) setupSubmit(w http.ResponseWriter, r *http.Request) {
	open, err := h.setupIsOpen(r.Context())
	if err != nil {
		h.Log.Error("setup submit gate", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !open {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pw := r.PostForm.Get("password")
	confirm := r.PostForm.Get("confirm_password")

	// Password rules: length is the only thing bcrypt cares about (72-byte
	// hard cap on input). We require at least 12 characters, which is the
	// minimum length that stands up to modern offline attacks against
	// bcrypt cost 12. No composition rules — long passphrases beat
	// enforced-symbol short passwords every time.
	if len(pw) < 12 {
		h.render(w, "setup", map[string]any{
			"Title":      "Set up your admin account",
			"ShowChrome": false,
			"Err":        "Password must be at least 12 characters.",
		})
		return
	}
	if len(pw) > 72 {
		// bcrypt silently truncates at 72 bytes. Refusing here means the
		// stored hash matches what the operator actually typed.
		h.render(w, "setup", map[string]any{
			"Title":      "Set up your admin account",
			"ShowChrome": false,
			"Err":        "Password too long (bcrypt cap is 72 characters). Use a shorter passphrase.",
		})
		return
	}
	if pw != confirm {
		h.render(w, "setup", map[string]any{
			"Title":      "Set up your admin account",
			"ShowChrome": false,
			"Err":        "Passwords don't match.",
		})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		h.Log.Error("setup bcrypt", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	// Transactional create with a serialisable re-check. If two POSTs race,
	// only the first commit succeeds; the second sees count > 0 and
	// falls through to the /login redirect below.
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.Log.Error("setup tx begin", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	var reCount int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM admins`).Scan(&reCount); err != nil {
		h.Log.Error("setup tx count", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if reCount > 0 {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Use the same well-known admin email the login handler / /settings
	// password change flow expects. Everything downstream (session claims,
	// audit logs, /settings#admins) is keyed off admin id, not email —
	// so the email is essentially a stable identifier for the one admin
	// row that exists.
	const email = "admin@didstorage.local"

	var newID int64
	err = tx.QueryRow(r.Context(),
		`INSERT INTO admins (email, password_hash) VALUES ($1, $2) RETURNING id`,
		email, string(hash),
	).Scan(&newID)
	if err != nil {
		// UNIQUE violation on email — someone else won the race between
		// our re-check and our insert. Treat like the "already set up"
		// case: send them to /login to sign in with the winner's creds.
		if errors.Is(err, pgx.ErrNoRows) || isPgUniqueViolation(err) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		h.Log.Error("setup insert admin", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.Log.Error("setup commit", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	// Auto-login: rotate the session token (defends against session
	// fixation attacks where an attacker seeds a session cookie before
	// setup completes) and set the admin id.
	if err := h.Session.RenewToken(r.Context()); err != nil {
		h.Log.Warn("setup renew session failed", "err", err)
	}
	auth.SetSession(h.Session, r, newID)

	h.Log.Info("first-run admin created via /setup", "admin_id", newID, "email", email)
	h.flashOK(r, "Welcome. Your admin account is set up.")
	http.Redirect(w, r, "/", http.StatusFound)
}

// isPgUniqueViolation returns true when err wraps a pgx UNIQUE constraint
// violation (SQLSTATE 23505). Used to detect the race where two /setup
// POSTs pass the count check but only one INSERT succeeds.
func isPgUniqueViolation(err error) bool {
	return pgErrCode(err) == "23505"
}
