// Package kyc holds path / filename helpers for KYC bundle file storage and
// the small "approve a bundle → activate dependent orders" transition logic.
//
// File layout on disk: KycRoot / {user_id} / {bundle_id} / {filename}
//
// Only the filename comes from the user; we sanitize it heavily to prevent
// path traversal. user_id and bundle_id come from our database.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// KycRoot is the base directory for KYC document storage. Override in tests.
var KycRoot = "/var/lib/didstorage/kyc"

// Pool is the minimal interface kyc helpers need from a pgxpool. Both
// *pgxpool.Pool and pgx.Tx satisfy this.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SanitizeFilename returns a safe basename for storage. We:
//   - strip any directory components
//   - keep only [a-zA-Z0-9._-]
//   - cap at 80 chars
//   - reject empty / dot-only names
func SanitizeFilename(in string) (string, error) {
	in = filepath.Base(in)
	var b strings.Builder
	for _, r := range in {
		switch {
		case unicode.IsLetter(r) && r < 128: // ASCII letters only
			b.WriteRune(r)
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		// keep the extension if any
		ext := filepath.Ext(out)
		if len(ext) <= 8 {
			out = out[:80-len(ext)] + ext
		} else {
			out = out[:80]
		}
	}
	out = strings.Trim(out, ".")
	if out == "" {
		return "", errors.New("filename is empty after sanitization")
	}
	return out, nil
}

// BundleDir returns the on-disk directory for one KYC bundle. Creates it if
// missing with mode 0750 (owner+group readable, world locked out).
func BundleDir(userID, bundleID int64) (string, error) {
	dir := filepath.Join(KycRoot, fmt.Sprintf("%d", userID), fmt.Sprintf("%d", bundleID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// DocPath returns the full path for a bundle document.
func DocPath(userID, bundleID int64, filename string) string {
	return filepath.Join(KycRoot, fmt.Sprintf("%d", userID), fmt.Sprintf("%d", bundleID), filename)
}

// ApproveBundle marks the bundle approved and (if any orders for the same user
// reference this bundle and are currently in kyc_pending) flips those orders
// to active. Returns the number of orders activated.
func ApproveBundle(ctx context.Context, tx Pool, bundleID, adminID int64) (int64, error) {
	var userID int64
	err := tx.QueryRow(ctx, `
		UPDATE kyc_bundles
		   SET status='approved', approved_by=$1, approved_at=now(), updated_at=now()
		 WHERE id=$2 AND status IN ('pending','rejected')
		 RETURNING user_id`, adminID, bundleID).Scan(&userID)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE orders
		   SET status='active'
		 WHERE kyc_bundle_id=$1 AND status='kyc_pending'`, bundleID)
	if err != nil {
		return 0, fmt.Errorf("activate dependent orders: %w", err)
	}
	_ = userID
	return tag.RowsAffected(), nil
}

// RejectBundle marks the bundle rejected with a reason. Dependent orders stay
// in kyc_pending — admin can attach a different bundle later.
func RejectBundle(ctx context.Context, tx Pool, bundleID, adminID int64, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE kyc_bundles
		   SET status='rejected', rejection_reason=$1, rejected_at=now(), updated_at=now()
		 WHERE id=$2 AND status IN ('pending','approved')`, reason, bundleID)
	return err
}
