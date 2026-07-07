// Package causes is the runtime cache of hangup-cause labels and tooltip
// detail strings used by the CDR list and SIP-trace pages. The source of
// truth lives in the hangup_causes table — admins edit rows there via the
// /cause-codes admin GUI, and the package keeps an in-memory snapshot for
// O(1) lookups on every CDR row render.
//
// Lookups stay non-blocking even during a Reload (RWMutex around a single
// map swap; the map itself is never mutated piecewise).
package causes

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Info struct {
	Code    string
	Label   string
	Detail  string
	Family  string // "sip" | "platform"
	Builtin bool
}

var (
	mu    sync.RWMutex
	table = map[string]Info{}
)

// Describe returns (label, detail) for a cause code. Empty detail signals
// "unknown code, render verbatim with no tooltip".
func Describe(code string) (string, string) {
	mu.RLock()
	defer mu.RUnlock()
	if info, ok := table[code]; ok {
		return info.Label, info.Detail
	}
	return code, ""
}

// IsPlatform reports whether `code` is one of our platform-side denial
// reasons (vs. a SIP-derived stack code). Used by the trace end-result
// formatter to colour the "rejected" pill.
func IsPlatform(code string) bool {
	mu.RLock()
	defer mu.RUnlock()
	if info, ok := table[code]; ok {
		return info.Family == "platform"
	}
	return false
}

// All returns a sorted snapshot of the cause table for the admin list page.
// We sort platform reasons first, then Q.850 numeric ascending.
func All() []Info {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Info, 0, len(table))
	for _, v := range table {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family == "platform" // platform first
		}
		// Within a family, compare numerically when both codes are
		// numeric, else alphabetically.
		ai, ae := parseInt(out[i].Code)
		bi, be := parseInt(out[j].Code)
		if ae == nil && be == nil {
			return ai < bi
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// Reload replaces the in-memory snapshot with the current DB contents.
// Call this at startup and after every successful write. Safe to call
// concurrently — readers see either the old or new map atomically.
func Reload(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT code, label, detail, family, builtin FROM hangup_causes`)
	if err != nil {
		return fmt.Errorf("query causes: %w", err)
	}
	defer rows.Close()
	next := make(map[string]Info, 64)
	for rows.Next() {
		var info Info
		if err := rows.Scan(&info.Code, &info.Label, &info.Detail, &info.Family, &info.Builtin); err != nil {
			return err
		}
		next[info.Code] = info
	}
	mu.Lock()
	table = next
	mu.Unlock()
	return nil
}

// Upsert inserts or updates a cause row, then refreshes the in-memory map.
// `family` must be "q850" or "platform".
func Upsert(ctx context.Context, pool *pgxpool.Pool, code, label, detail, family string) error {
	if code == "" || label == "" {
		return fmt.Errorf("code and label are required")
	}
	if family != "sip" && family != "platform" {
		return fmt.Errorf("family must be 'sip' or 'platform'")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO hangup_causes (code, label, detail, family, builtin)
		VALUES ($1, $2, $3, $4, false)
		ON CONFLICT (code) DO UPDATE SET
		  label      = EXCLUDED.label,
		  detail     = EXCLUDED.detail,
		  family     = EXCLUDED.family,
		  updated_at = now()`,
		code, label, detail, family)
	if err != nil {
		return err
	}
	return Reload(ctx, pool)
}

// Delete removes a non-builtin cause. Built-in causes are protected — admins
// can edit their wording but not lose the row entirely (the system still
// emits these codes from sipctl regardless of what the table says).
func Delete(ctx context.Context, pool *pgxpool.Pool, code string) error {
	tag, err := pool.Exec(ctx,
		`DELETE FROM hangup_causes WHERE code=$1 AND builtin=false`, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cause not found, or it's a built-in (edit instead of delete)")
	}
	return Reload(ctx, pool)
}

func parseInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
