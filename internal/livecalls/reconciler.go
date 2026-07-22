package livecalls

// Background reconciler for the live-calls index.
//
// Why this exists
// ─────────────────────────────────────────────────────────────
// The primary cleanup path is dialplan-driven: Asterisk's hangup handler
// runs dids-cdr.py, which POSTs /sipctl/cdr, which calls Deregister. That
// works most of the time.
//
// Failure modes we've observed in production:
//
//   1. dids-cdr.py POST times out (3s deadline) — didapi mid-restart, a
//      DB stall on the ledger write, urllib error, whatever. AGI logs to
//      stderr and returns; Asterisk swallows stderr; Deregister never runs.
//   2. ${CDR(userfield)} is unset in some dialplan exit paths (spawn-error
//      from a broken Playback, transferred channels leaving the hangup
//      handler context). AGI receives an empty call_id and no-ops.
//   3. Asterisk drops the channel abruptly (crashing module, signal). The
//      "h" fallback and pushed hangup handler both get skipped.
//
// In all three cases the call has genuinely ended on the box — the
// PJSIP channel is gone from `core show channels` — but the Redis
// ZSET + meta blob for it lingers for the full defaultTTL (4h) window.
// The /live table shows a phantom "34-min-old Active" row; the
// act:user:* / act:did:* SETs still contain the call_id so subsequent
// admissions run against an inflated concurrency count and can wrongly
// 503 with insufficient_channels.
//
// The reconciler runs every tickInterval, gets Asterisk's authoritative
// live-channel set, and evicts any live-calls row whose channel is no
// longer alive there. This is the same logic the /live "Force cleanup"
// button ran manually, elevated to an automatic sweep.
//
// Settle window
// ─────────────────────────────────────────────────────────────
// A call registers in Redis before Asterisk necessarily has its channel
// visible in `core show channels concise` (the AGI runs mid-dialplan,
// Redis is fast, Asterisk's channel list snapshot takes a beat). We
// require a call to be at least settleWindow old before considering it
// for eviction, so a brand-new call never gets swept during its own
// admission race.

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/redis/go-redis/v9"
)

// ChannelSetFunc returns the current set of live Asterisk channel names,
// keyed by name (value is unused). Split out as an argument so tests can
// inject a fake without spawning a real `asterisk -rx`.
type ChannelSetFunc func(ctx context.Context) map[string]struct{}

// DefaultChannelSet queries the local Asterisk via `asterisk -rx "core
// show channels concise"`, exactly what channelStates in the web package
// uses. Returns nil on any failure — the caller treats nil as "unknown"
// and does NOT sweep (better to leave a ghost than falsely evict a real
// call during an Asterisk restart).
func DefaultChannelSet(ctx context.Context) map[string]struct{} {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "asterisk", "-rx", "core show channels concise").Output()
	if err != nil {
		return nil
	}
	m := map[string]struct{}{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := bytes.Split(line, []byte("!"))
		if len(fields) < 5 {
			continue
		}
		name := strings.TrimSpace(string(fields[0]))
		if name == "" {
			continue
		}
		m[name] = struct{}{}
	}
	return m
}

// ReconcilerOptions configures the background sweep. Zero-valued fields
// take documented defaults.
type ReconcilerOptions struct {
	// TickInterval is how often the sweep runs. Default 15s. Shorter =
	// faster ghost eviction, more asterisk -rx forks. 15s is a good
	// balance: worst-case ghost visibility ≈ 15-30s.
	TickInterval time.Duration
	// SettleWindow is how young a call has to be before it's eligible
	// for eviction — protects against the register-then-immediately-
	// sweep race. Default 20s.
	SettleWindow time.Duration
	// ChannelSet returns the current Asterisk channel set. Default
	// DefaultChannelSet.
	ChannelSet ChannelSetFunc
	// ReleaseReservations is called with the list of evicted call_ids
	// after each successful sweep, so the caller can SREM them from the
	// act:user:* / act:did:* concurrency sets. Optional — omit if the
	// caller only cares about the visibility index.
	ReleaseReservations func(ctx context.Context, callIDs []string) error
	// DB is the Postgres pool used to stamp cdrs.ended_at on ghost rows
	// where /sipctl/cdr never fired. When set, the reconciler sets
	// ended_at = now() and billsec = extract(epoch from now() - started_at)
	// on every UPDATE, so downstream billing / duration displays reflect
	// a value within tickInterval seconds of the real hangup instead of
	// staying NULL forever (which was showing as an infinite duration
	// on the /cdrs list). Only rows where ended_at IS NULL are touched.
	DB *pgxpool.Pool
	// Log receives structured events for every non-trivial sweep. If
	// nil, sweeps are silent.
	Log *slog.Logger
}

// StartReconciler launches a goroutine that runs the sweep on tick.
// Returns immediately. The goroutine stops when ctx is cancelled.
func StartReconciler(ctx context.Context, rdb *redis.Client, opts ReconcilerOptions) {
	if opts.TickInterval <= 0 {
		opts.TickInterval = 15 * time.Second
	}
	// SettleWindow < 0 → default 20s. Exactly 0 is a legitimate caller
	// choice ("no grace period, sweep everything on every tick") and
	// must be preserved.
	if opts.SettleWindow < 0 {
		opts.SettleWindow = 20 * time.Second
	}
	if opts.ChannelSet == nil {
		opts.ChannelSet = DefaultChannelSet
	}

	go func() {
		t := time.NewTicker(opts.TickInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runReconcileOnce(ctx, rdb, opts)
			}
		}
	}()
}

func runReconcileOnce(ctx context.Context, rdb *redis.Client, opts ReconcilerOptions) {
	chans := opts.ChannelSet(ctx)
	if chans == nil {
		// Asterisk unavailable — don't sweep. A brief Asterisk outage
		// shouldn't turn every legitimate live call into a ghost.
		if opts.Log != nil {
			opts.Log.Debug("livecalls reconciler skipped: asterisk unreachable")
		}
		return
	}

	calls, err := List(ctx, rdb)
	if err != nil {
		if opts.Log != nil {
			opts.Log.Warn("livecalls reconciler list failed", "err", err)
		}
		return
	}
	if len(calls) == 0 {
		return
	}

	cutoff := time.Now().Add(-opts.SettleWindow).Unix()
	var evicted []string
	for _, ac := range calls {
		// Too young — protects against a Register-then-sweep race where
		// the AGI has hit /sipctl/authorize but Asterisk hasn't yet
		// bound the channel into `core show channels`.
		if ac.StartedAt > cutoff {
			continue
		}
		// Never had a channel name in the first place (synthetic probe
		// hitting /sipctl/authorize by hand) — always a ghost.
		if ac.AsteriskChannel == "" {
			evicted = append(evicted, ac.CallID)
			continue
		}
		if _, alive := chans[ac.AsteriskChannel]; !alive {
			evicted = append(evicted, ac.CallID)
		}
	}
	if len(evicted) == 0 {
		return
	}

	for _, cid := range evicted {
		_ = Deregister(ctx, rdb, cid)
	}
	if opts.ReleaseReservations != nil {
		if err := opts.ReleaseReservations(ctx, evicted); err != nil && opts.Log != nil {
			opts.Log.Warn("livecalls reconciler release-reservations failed", "err", err)
		}
	}

	// Stamp cdrs.ended_at on any shell rows that don't have it yet. We
	// know the underlying Asterisk channel just died (this tick is the
	// first to observe it gone), so now() is within tickInterval of the
	// true hangup — way better than leaving ended_at NULL forever, which
	// on the /cdrs list rendered as multi-minute Durations that didn't
	// match the caller's own CDRs. Silent no-op when DB isn't wired in.
	if opts.DB != nil {
		if _, err := opts.DB.Exec(ctx, `
			UPDATE cdrs
			   SET ended_at = now(),
			       billsec = GREATEST(0, EXTRACT(EPOCH FROM now() - started_at)::int)
			 WHERE call_id = ANY($1) AND ended_at IS NULL`,
			evicted); err != nil && opts.Log != nil {
			opts.Log.Warn("livecalls reconciler cdr timestamp fill failed", "err", err)
		}
	}

	if opts.Log != nil {
		opts.Log.Info("livecalls reconciler swept ghost entries",
			"evicted_count", len(evicted),
			"live_count", len(calls)-len(evicted),
			"call_ids", strings.Join(evicted, ","))
	}
}

// ReleaseChannelReservations SCANs every act:* SET in Redis and SREMs each
// provided call_id from it. Called after the reconciler evicts ghost
// entries so the corresponding concurrency-cap SETs (act:user:<id>,
// act:did:<id>) don't stay over-counted and cause spurious
// insufficient_channels denials.
//
// Bounded SCAN cursor (200 keys per page) + pipelined SREMs, so worst
// case is one Redis round trip per page.
func ReleaseChannelReservations(ctx context.Context, rdb *redis.Client, callIDs []string) error {
	if len(callIDs) == 0 {
		return nil
	}
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "act:*", 200).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			pipe := rdb.Pipeline()
			for _, k := range keys {
				for _, cid := range callIDs {
					pipe.SRem(ctx, k, cid)
				}
			}
			if _, err := pipe.Exec(ctx); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}
