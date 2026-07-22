// Package livecalls is a tiny Redis-backed index of in-flight calls.
//
// Lifecycle:
//   - /sipctl/authorize:allow  → Register(call_id, meta)
//   - /sipctl/cdr              → Deregister(call_id)
//   - /live page               → List() (newest first)
//
// Storage layout:
//
//	live:active                   ZSET, score = unix seconds at /authorize
//	                              member  = sanitized call_id
//	live:meta:<sanitized_call_id> STRING (JSON of ActiveCall), TTL 4h
//
// The ZSET is the source of truth for "what's currently live"; the per-call
// hash carries the metadata needed to render the table and act on the call.
// We rely on the TTL on `live:meta:*` plus a sweep-on-list to evict entries
// the BYE somehow missed (cleanup runs every list call — cheap, bounded).
package livecalls

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	activeKey  = "live:active"
	metaPrefix = "live:meta:"
	// defaultTTL is the ceiling for a live-call meta blob. Real calls are
	// bounded above by the platform's balance-derived max seconds (usually
	// <10 min) and the runaway L() timeout on the Dial (1h). 2h is a
	// generous multiple of both — the reconciler goroutine
	// (internal/livecalls/reconciler.go) evicts ghost rows within ~15s of
	// the underlying channel dying, so this TTL is only reached by a call
	// that is somehow both truly live AND has no matching Asterisk channel
	// (impossible in practice).
	defaultTTL = 2 * time.Hour
	// sweepWindow is the age past which List() drops ZSET entries whose
	// meta key has expired. Same reasoning as defaultTTL: the reconciler
	// beats this by orders of magnitude on the happy path.
	sweepWindow = 2 * time.Hour
)

// ActiveCall is the metadata captured at /authorize-allow time. Re-marshalled
// on every page render — keep it small. Only what the live table and the
// hangup action need.
type ActiveCall struct {
	CallID         string  `json:"call_id"`           // sanitized (no @host:port)
	CallIDFull     string  `json:"call_id_full"`      // original, includes @host:port; used to match in Asterisk
	StartedAt      int64   `json:"started_at_unix"`   // unix seconds
	UserID         int64   `json:"user_id,omitempty"`
	OrderID        int64   `json:"order_id,omitempty"`
	DIDID          int64   `json:"did_id,omitempty"`
	SupplierID     int64   `json:"supplier_id"`
	E164           string  `json:"e164,omitempty"`    // dialled DID (the "To" side)
	SrcIP          string  `json:"src_ip"`            // supplier IP (the "From" side)
	SrcURI         string  `json:"src_uri,omitempty"` // caller URI as observed
	RouteKind      string  `json:"route_kind"`
	RouteTarget    string  `json:"route_target"`
	Reserved       bool    `json:"reserved,omitempty"`       // true for status='reserved' DID short-circuit
	RatePerMinCents float64 `json:"rate_per_min_cents,omitempty"`

	// AsteriskChannel is the exact channel name from Asterisk
	// (e.g. "PJSIP/globetelecom-00000063"), captured at /sipctl/authorize
	// time from the dialplan's ${CHANNEL} variable. /live admin actions
	// use it directly to hang up / transfer the right channel, without
	// having to grep `pjsip show channels` by Exten. This survives a
	// redirect — when the channel's transferred into [admin-actions] its
	// Exten changes but the channel name does not.
	AsteriskChannel string `json:"asterisk_channel,omitempty"`

	// LastAdminAction is set when an admin acted on this call mid-flight
	// from /live and the call is still alive afterwards. Today only redirect
	// fits that shape — warn and hangup end the call so they Deregister
	// instead of stamping this. Empty by default; the /live UI surfaces
	// this as a pill on the row so the admin sees the row has been acted
	// on without it disappearing.
	LastAdminAction string `json:"last_admin_action,omitempty"`
}

// Register marks a call as live. Idempotent — a duplicate /authorize on the
// same call_id overwrites the existing metadata (re-INVITE / retransmit).
func Register(ctx context.Context, rdb *redis.Client, c ActiveCall) error {
	if c.CallID == "" {
		return errors.New("empty call_id")
	}
	if c.StartedAt == 0 {
		c.StartedAt = time.Now().UTC().Unix()
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	pipe := rdb.TxPipeline()
	pipe.Set(ctx, metaPrefix+c.CallID, blob, defaultTTL)
	pipe.ZAdd(ctx, activeKey, redis.Z{Score: float64(c.StartedAt), Member: c.CallID})
	_, err = pipe.Exec(ctx)
	return err
}

// Deregister clears the call from both the index and the metadata cache. Safe
// to call on an unknown call_id (no-op).
func Deregister(ctx context.Context, rdb *redis.Client, callID string) error {
	if callID == "" {
		return nil
	}
	pipe := rdb.TxPipeline()
	pipe.ZRem(ctx, activeKey, callID)
	pipe.Del(ctx, metaPrefix+callID)
	_, err := pipe.Exec(ctx)
	return err
}

// List returns every currently-tracked active call, newest-first. Also
// sweeps stale entries (ZSET members whose meta key has expired) so a Redis
// flush or a crashed didapi doesn't leave dangling rows visible to admins
// forever.
func List(ctx context.Context, rdb *redis.Client) ([]ActiveCall, error) {
	// Newest first — Redis ZRange with Rev=true on the score range works.
	members, err := rdb.ZRevRangeWithScores(ctx, activeKey, 0, 500).Result()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}
	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = metaPrefix + m.Member.(string)
	}
	vals, err := rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	out := make([]ActiveCall, 0, len(vals))
	var staleMembers []any
	cutoff := time.Now().UTC().Add(-sweepWindow).Unix()
	for i, raw := range vals {
		if raw == nil {
			// Meta expired but ZSET still references it; sweep it.
			staleMembers = append(staleMembers, members[i].Member)
			continue
		}
		var ac ActiveCall
		s, _ := raw.(string)
		if err := json.Unmarshal([]byte(s), &ac); err != nil {
			staleMembers = append(staleMembers, members[i].Member)
			continue
		}
		if ac.StartedAt < cutoff {
			staleMembers = append(staleMembers, members[i].Member)
			continue
		}
		out = append(out, ac)
	}
	if len(staleMembers) > 0 {
		// Fire-and-forget cleanup; we don't want list to fail because of it.
		_, _ = rdb.ZRem(ctx, activeKey, staleMembers...).Result()
	}
	return out, nil
}

// UpdateRoute swaps route_kind / route_target on an in-flight call AND
// stamps last_admin_action. Used by /live/{call_id}/redirect after a
// successful channel transfer to a new destination — the call stays in
// the live index, but the displayed route reflects what Asterisk is now
// bridged to. No-op if the call isn't tracked (already deregistered,
// never registered, etc.).
//
// The TTL gets refreshed to defaultTTL so a long-running redirected call
// doesn't expire mid-bridge.
func UpdateRoute(ctx context.Context, rdb *redis.Client, callID, routeKind, routeTarget, lastAction string) error {
	if callID == "" {
		return nil
	}
	ac, err := Get(ctx, rdb, callID)
	if err != nil {
		return err
	}
	if ac == nil {
		return nil
	}
	ac.RouteKind = routeKind
	ac.RouteTarget = routeTarget
	ac.LastAdminAction = lastAction
	blob, err := json.Marshal(ac)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, metaPrefix+callID, blob, defaultTTL).Err()
}

// Get returns metadata for a single call_id, or (nil, nil) if not tracked.
func Get(ctx context.Context, rdb *redis.Client, callID string) (*ActiveCall, error) {
	raw, err := rdb.Get(ctx, metaPrefix+callID).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ac ActiveCall
	if err := json.Unmarshal([]byte(raw), &ac); err != nil {
		return nil, err
	}
	return &ac, nil
}

// CallAge returns the wall-clock duration since the call started. Helper for
// rendering the live table without scattering time arithmetic everywhere.
func (c ActiveCall) Age() time.Duration {
	if c.StartedAt == 0 {
		return 0
	}
	return time.Since(time.Unix(c.StartedAt, 0))
}

// AgeDisplay formats the call duration as "1h 23m" / "45s" / "12m 03s" —
// matches the compactDuration helper used elsewhere in the admin GUI.
func (c ActiveCall) AgeDisplay() string {
	d := c.Age()
	s := int(d.Seconds())
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return strconv.Itoa(s) + "s"
	case s < 3600:
		return strconv.Itoa(s/60) + "m " + strconv.Itoa(s%60) + "s"
	default:
		return strconv.Itoa(s/3600) + "h " + strconv.Itoa((s%3600)/60) + "m"
	}
}
