// Package auth holds the small set of admin auth primitives: password hashing,
// admin upsert from env, and the chi-style middleware that gates GUI routes.
package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionAdminKey = "admin_id"
	defaultAdmin    = "admin@didstorage.local"
)

// EnsureAdmin inserts/updates the single admin from env. Idempotent.
// If the admin already exists, its password is updated to match.
func EnsureAdmin(ctx context.Context, pg *pgxpool.Pool, password string) error {
	if password == "" {
		return errors.New("ADMIN_PASSWORD is empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = pg.Exec(ctx, `
		INSERT INTO admins (email, password_hash) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash
	`, defaultAdmin, string(hash))
	return err
}

// Verify checks the password against the stored hash and, on success, returns
// the admin id. ErrInvalid otherwise (regardless of which field was wrong).
var ErrInvalid = errors.New("invalid login")

func Verify(ctx context.Context, pg *pgxpool.Pool, password string) (int64, error) {
	var (
		id   int64
		hash string
	)
	err := pg.QueryRow(ctx, `SELECT id, password_hash FROM admins WHERE email = $1`, defaultAdmin).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInvalid
	}
	if err != nil {
		return 0, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return 0, ErrInvalid
	}
	return id, nil
}

func SetSession(sm *scs.SessionManager, r *http.Request, adminID int64) {
	sm.Put(r.Context(), sessionAdminKey, adminID)
}

func ClearSession(sm *scs.SessionManager, r *http.Request) {
	sm.Remove(r.Context(), sessionAdminKey)
}

func IsLoggedIn(sm *scs.SessionManager, r *http.Request) bool {
	return sm.GetInt64(r.Context(), sessionAdminKey) != 0
}

// AdminIDFromSession returns the logged-in admin's id, or 0 if absent. Used by
// audit-log writes (KYC approvals, user blocks) so we can record who did what.
func AdminIDFromSession(sm *scs.SessionManager, r *http.Request) int64 {
	return sm.GetInt64(r.Context(), sessionAdminKey)
}

// RequireAdmin is the middleware that protects all admin GUI routes.
// Unauthenticated requests are redirected to /login.
func RequireAdmin(sm *scs.SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !IsLoggedIn(sm, r) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
