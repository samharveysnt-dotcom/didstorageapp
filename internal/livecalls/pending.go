package livecalls

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Pending admin actions buffer the {action, admin, reason} for a call that
// has been live-actioned (Hangup/Warn/Redirect) but whose CDR row hasn't
// been written yet. /sipctl/cdr pops the entry when it fires and stamps the
// admin_action* columns on the cdrs row in the same INSERT.
//
// Storage:
//   pending:admin_action:<sanitized_call_id> → JSON, TTL 6h
//
// 6h TTL is wider than the 4h live-call TTL so we don't lose the marker on
// a long hold-and-then-end scenario. Worst case: an entry orphans if the
// channel was hung up admin-side but /cdr never fires (Asterisk crashed
// mid-call) — Redis evicts it after 6h.

const pendingPrefix = "pending:admin_action:"
const pendingTTL = 6 * time.Hour

type PendingAction struct {
	Action  string `json:"action"`            // "live_hangup" | "live_warn" | "live_redirect"
	AdminID int64  `json:"admin_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// RecordPendingAction stores the action so /sipctl/cdr can pick it up.
// Idempotent — re-recording the same call_id overwrites (most recent
// action wins; if an admin warns then redirects the same call before the
// CDR fires, the redirect mark is what shows up).
func RecordPendingAction(ctx context.Context, rdb *redis.Client, callID, action string, adminID int64, reason string) error {
	if callID == "" || action == "" {
		return errors.New("empty call_id or action")
	}
	blob, err := json.Marshal(PendingAction{Action: action, AdminID: adminID, Reason: reason})
	if err != nil {
		return err
	}
	return rdb.Set(ctx, pendingPrefix+callID, blob, pendingTTL).Err()
}

// PopPendingAction reads-and-deletes the entry. Returns (nil, nil) if no
// entry exists (the common case — most calls aren't admin-actioned).
// /sipctl/cdr calls this once per CDR insert; the pop semantics prevent a
// second CDR for the same call_id from claiming the same action mark.
func PopPendingAction(ctx context.Context, rdb *redis.Client, callID string) (*PendingAction, error) {
	if callID == "" {
		return nil, nil
	}
	raw, err := rdb.GetDel(ctx, pendingPrefix+callID).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingAction
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
