package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"didstorage/internal/auth"
	"didstorage/internal/settings"
	"didstorage/internal/sslmgr"
)

// settingsPage renders the /settings tabbed page (Company / Site / Admins /
// Domains & SSL). All four panes are rendered in one round-trip so deep-link
// hashes (#admins / #domains) work without an extra request.
func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	type adminRow struct {
		ID      int64
		Email   string
		Created string
	}
	var admins []adminRow
	rows, _ := h.DB.Query(r.Context(),
		`SELECT id, email, to_char(created_at,'YYYY-MM-DD HH24:MI') FROM admins ORDER BY id`)
	for rows.Next() {
		var a adminRow
		rows.Scan(&a.ID, &a.Email, &a.Created)
		admins = append(admins, a)
	}
	rows.Close()

	var sslDomains []sslmgr.Domain
	if h.SSL != nil {
		sslDomains = h.SSL.Domains()
	}

	ok, em := h.popFlashes(r)
	h.render(w, "settings", map[string]any{
		"Title":          "Settings",
		"Section":        "settings",
		"FlashOK":        ok,
		"FlashErr":       em,
		"Company":        settings.ByCategory("company"),
		"Site":           settings.ByCategory("site"),
		"Admins":         admins,
		"Domains":        sslDomains,
		"HTTPSConfigured": h.SSL != nil && h.SSL.IsConfigured(),
	})
}

// settingsBulkUpdate handles POSTs from either the Company or Site form.
// Each visible setting is rendered as <input name="kv:<key>" value="…">; we
// persist anything that came in with that prefix and matches a known key.
// Unknown keys are ignored (defensive — keeps the form from creating rogue
// rows on stale templates).
func (h *Handler) settingsBulkUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}
	known := map[string]bool{}
	for _, s := range settings.All() {
		known[s.Key] = true
	}
	saved := 0
	for k, vs := range r.PostForm {
		if !strings.HasPrefix(k, "kv:") || len(vs) == 0 {
			continue
		}
		key := strings.TrimPrefix(k, "kv:")
		if !known[key] {
			continue
		}
		if err := settings.Set(r.Context(), h.DB.Pool, key, vs[0]); err != nil {
			h.flashErr(r, "save '"+key+"': "+err.Error())
			http.Redirect(w, r, "/settings", http.StatusFound)
			return
		}
		saved++
	}
	h.flashOK(r, fmt.Sprintf("Saved %d setting(s)", saved))
	http.Redirect(w, r, "/settings#"+r.URL.Query().Get("tab"), http.StatusFound)
}

// adminPasswordChange validates the current password (so a stolen session
// alone can't lock the legitimate admin out) and updates the bcrypt hash.
// Email is not editable from the GUI — auth.go uses a hardcoded default
// email, so changing it would lock everyone out.
func (h *Handler) adminPasswordChange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	current := r.PostForm.Get("current")
	next := r.PostForm.Get("new_password")
	confirm := r.PostForm.Get("confirm_password")
	if next == "" || len(next) < 8 {
		h.flashErr(r, "new password must be at least 8 characters")
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	if next != confirm {
		h.flashErr(r, "new password and confirmation don't match")
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	// Verify the current password.
	if _, err := auth.Verify(r.Context(), h.DB.Pool, current); err != nil {
		h.flashErr(r, "current password is incorrect")
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		h.flashErr(r, "hash: "+err.Error())
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	if _, err := h.DB.Exec(r.Context(),
		`UPDATE admins SET password_hash=$1 WHERE id=$2`,
		string(hash), auth.AdminIDFromSession(h.Session, r)); err != nil {
		h.flashErr(r, "save: "+err.Error())
		http.Redirect(w, r, "/settings#admins", http.StatusFound)
		return
	}
	h.flashOK(r, "Password changed.")
	http.Redirect(w, r, "/settings#admins", http.StatusFound)
}

// domainCreate adds a hostname row (cert/key optional — admin can paste them
// later via domainUpdate).
func (h *Handler) domainCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	host := strings.TrimSpace(strings.ToLower(r.PostForm.Get("hostname")))
	if host == "" {
		h.flashErr(r, "hostname is required")
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	notes := strings.TrimSpace(r.PostForm.Get("notes"))
	_, err := h.DB.Exec(r.Context(),
		`INSERT INTO site_domains (hostname, notes) VALUES ($1, $2)
		 ON CONFLICT (hostname) DO NOTHING`, host, notes)
	if err != nil {
		h.flashErr(r, "create: "+err.Error())
	} else {
		h.flashOK(r, "Domain '"+host+"' added — paste a cert+key next.")
	}
	http.Redirect(w, r, "/settings#domains", http.StatusFound)
}

// domainUpdate accepts cert PEM + key PEM (optional) plus a default flag and
// notes. Validates the PEM up front so the admin gets a meaningful error
// instead of waiting for the next sslmgr.Reload to skip it.
func (h *Handler) domainUpdate(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := r.ParseForm(); err != nil {
		h.flashErr(r, "bad form")
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	notes := strings.TrimSpace(r.PostForm.Get("notes"))
	isDefault := r.PostForm.Get("is_default") == "1"
	certPEM := strings.TrimSpace(r.PostForm.Get("cert_pem"))
	keyPEM := strings.TrimSpace(r.PostForm.Get("key_pem"))

	var subject, issuer string
	var notAfter *time.Time
	if certPEM != "" {
		meta, err := sslmgr.ParseCertPEM([]byte(certPEM))
		if err != nil {
			h.flashErr(r, "certificate PEM: "+err.Error())
			http.Redirect(w, r, "/settings#domains", http.StatusFound)
			return
		}
		subject = meta.Subject
		issuer = meta.Issuer
		notAfter = &meta.NotAfter
		if keyPEM == "" {
			h.flashErr(r, "you supplied a certificate but no private key")
			http.Redirect(w, r, "/settings#domains", http.StatusFound)
			return
		}
		if err := sslmgr.ValidateKeyPEM([]byte(keyPEM)); err != nil {
			h.flashErr(r, "private key PEM: "+err.Error())
			http.Redirect(w, r, "/settings#domains", http.StatusFound)
			return
		}
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	defer tx.Rollback(r.Context())

	if isDefault {
		// At most one default — clear the rest first.
		if _, err := tx.Exec(r.Context(),
			`UPDATE site_domains SET is_default=false WHERE id<>$1`, id); err != nil {
			h.flashErr(r, "clear default: "+err.Error())
			http.Redirect(w, r, "/settings#domains", http.StatusFound)
			return
		}
	}

	// Only overwrite cert/key columns if PEM was provided — empty submission
	// is "edit notes / default flag", not "wipe my cert".
	if certPEM != "" {
		_, err = tx.Exec(r.Context(), `
			UPDATE site_domains
			   SET cert_pem=$1, key_pem=$2,
			       cert_subject=$3, cert_issuer=$4, cert_expires_at=$5,
			       notes=$6, is_default=$7, updated_at=now()
			 WHERE id=$8`,
			certPEM, keyPEM, subject, issuer, notAfter, notes, isDefault, id)
	} else {
		_, err = tx.Exec(r.Context(), `
			UPDATE site_domains
			   SET notes=$1, is_default=$2, updated_at=now()
			 WHERE id=$3`, notes, isDefault, id)
	}
	if err != nil {
		h.flashErr(r, "save: "+err.Error())
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.flashErr(r, err.Error())
		http.Redirect(w, r, "/settings#domains", http.StatusFound)
		return
	}
	if h.SSL != nil {
		if err := h.SSL.Reload(r.Context()); err != nil {
			h.Log.Warn("sslmgr reload after domain update failed", "err", err)
		}
	}
	h.flashOK(r, "Domain saved.")
	http.Redirect(w, r, "/settings#domains", http.StatusFound)
}

// domainDelete drops the row. Cert lives in DB so this also invalidates any
// in-memory cache via sslmgr.Reload().
func (h *Handler) domainDelete(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	tag, err := h.DB.Exec(r.Context(), `DELETE FROM site_domains WHERE id=$1`, id)
	if err != nil {
		h.flashErr(r, "delete: "+err.Error())
	} else if tag.RowsAffected() == 0 {
		h.flashErr(r, "domain not found")
	} else {
		h.flashOK(r, "Domain deleted")
		if h.SSL != nil {
			_ = h.SSL.Reload(r.Context())
		}
	}
	http.Redirect(w, r, "/settings#domains", http.StatusFound)
}

// _ silences unused-import warnings if a path is taken out later. chi/pgx
// stay legitimate via the route binder + DB access.
var _ = chi.URLParam
var _ = errors.New
var _ = pgx.ErrNoRows
