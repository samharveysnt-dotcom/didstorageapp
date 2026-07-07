// Package audiogroup owns the per-call clip-selection logic for the
// audio_group route kind. Shared by:
//
//   - internal/sipctl    — called from /sipctl/authorize on every INVITE
//                          that resolves to an audio_group (reservations
//                          + customer orders).
//   - internal/web       — called from /live/{id}/redirect when an admin
//                          redirects a live call to play a clip from a
//                          group instead of a SIP target.
//
// Selection is random-no-repeat: uniformly random across the group's
// current members, excluding whichever clip played most recently for
// that same group. The "last played" id lives in Redis under
// `audio_group_last:<group_id>` with a 5-minute TTL — bursts of calls in
// a short window keep rotating; idle periods naturally reset the
// exclusion so a single-member group still works after a quiet hour.
//
// Empty groups return an error; single-member groups bypass the Redis
// lookup and return that one member.
package audiogroup

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"didstorage/internal/db"
)

// lastPlayedTTL bounds how long a "recently-played" exclusion is honoured.
// Tuned long enough to ride out a burst of consecutive calls (~tens per
// minute), short enough that a single-member group still works after a
// quiet hour without the lone clip being eternally excluded by its own
// ghost.
const lastPlayedTTL = 5 * time.Minute

// PickMember selects one audio_files row from group groupID and returns
// its on-disk basename (suitable for prefixing with "didstorage/" to
// produce an Asterisk Playback target). Selection is random-no-repeat
// using a per-group last-played key in Redis.
//
// Returns an error when the group has zero members or the DB lookup
// fails. Redis errors are non-fatal — a hiccup just disables the no-
// repeat guard for the duration of the outage; the caller still gets a
// valid pick.
func PickMember(ctx context.Context, pgdb *db.DB, rdb *redis.Client, groupID int64) (string, error) {
	rows, err := pgdb.Query(ctx, `
		SELECT m.audio_file_id, af.filename
		  FROM audio_group_members m
		  JOIN audio_files af ON af.id = m.audio_file_id
		 WHERE m.group_id = $1
		 ORDER BY m.position, m.audio_file_id`, groupID)
	if err != nil {
		return "", fmt.Errorf("audio_group members query: %w", err)
	}
	defer rows.Close()
	type member struct {
		id       int64
		filename string
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.filename); err != nil {
			return "", fmt.Errorf("audio_group members scan: %w", err)
		}
		members = append(members, m)
	}
	if len(members) == 0 {
		return "", fmt.Errorf("audio_group %d has no members", groupID)
	}
	if len(members) == 1 {
		return members[0].filename, nil
	}

	redisKey := fmt.Sprintf("audio_group_last:%d", groupID)
	lastID, _ := rdb.Get(ctx, redisKey).Int64()

	candidates := members
	if lastID > 0 {
		filtered := make([]member, 0, len(members))
		for _, m := range members {
			if m.id != lastID {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	// crypto/rand would be overkill for "which canned clip to play".
	// time-based seed gives an indistinguishable uniform distribution.
	idx := int(time.Now().UnixNano()/1000) % len(candidates)
	if idx < 0 {
		idx = -idx
	}
	picked := candidates[idx]
	_ = rdb.Set(ctx, redisKey, picked.id, lastPlayedTTL).Err()
	return picked.filename, nil
}
