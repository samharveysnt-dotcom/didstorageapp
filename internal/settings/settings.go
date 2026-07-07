// Package settings is a simple DB-backed key-value store with an in-memory
// snapshot. Mirrors the pattern in internal/causes — admins edit values via
// the /settings admin GUI; we keep an RWMutex-protected map for O(1) lookups
// from anywhere in the app.
//
// Reload runs at startup and after every successful write. Get() / GetInt()
// fall back to the supplied default when the key is missing.
package settings

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Setting struct {
	Key         string
	Value       string
	Category    string
	Description string
}

var (
	mu    sync.RWMutex
	cache = map[string]Setting{}
)

// Get returns the current value for key, or "" if absent.
func Get(key string) string {
	mu.RLock()
	defer mu.RUnlock()
	if s, ok := cache[key]; ok {
		return s.Value
	}
	return ""
}

// GetWithDefault returns the value for key, or def if missing / blank.
func GetWithDefault(key, def string) string {
	v := Get(key)
	if v == "" {
		return def
	}
	return v
}

// GetInt parses an integer setting; returns def if missing / unparseable.
func GetInt(key string, def int) int {
	v := Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// All returns a sorted snapshot grouped by (category, key) for the admin UI.
func All() []Setting {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Setting, 0, len(cache))
	for _, v := range cache {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ByCategory returns just the settings in one category, sorted by key.
func ByCategory(category string) []Setting {
	mu.RLock()
	defer mu.RUnlock()
	var out []Setting
	for _, v := range cache {
		if v.Category == category {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Reload replaces the in-memory snapshot with the current DB contents.
// Atomic from the readers' point of view (they always see a complete map).
func Reload(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT key, value, category, description FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := make(map[string]Setting, 32)
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Key, &s.Value, &s.Category, &s.Description); err != nil {
			return err
		}
		next[s.Key] = s
	}
	mu.Lock()
	cache = next
	mu.Unlock()
	return nil
}

// Set writes a key (creates if missing, updates if present) and reloads.
// `description` and `category` are only applied on insert; subsequent calls
// update only the value. This keeps admin-configurable strings from
// accidentally re-categorizing themselves.
func Set(ctx context.Context, pool *pgxpool.Pool, key, value string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO settings (key, value, category, description)
		VALUES ($1, $2, 'general', '')
		ON CONFLICT (key) DO UPDATE SET
		  value      = EXCLUDED.value,
		  updated_at = now()`, key, value)
	if err != nil {
		return err
	}
	return Reload(ctx, pool)
}
