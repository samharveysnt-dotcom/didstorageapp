package didsimport

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the minimal interface the worker needs from the application's
// pgxpool wrapper. Lets the package be unit-tested without spinning up
// a real Postgres.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Run executes the import in the current goroutine — callers normally spawn
// it as `go didsimport.Run(...)`. It walks entries in order, attempts an
// idempotent INSERT for each, and emits a Row event per outcome plus a
// throttled Progress event roughly every 250ms.
//
// Required arguments:
//
//   - ctx: cancellation context. The worker checks it between rows so a
//     parent shutdown finishes within one row of work.
//   - db: the pgx pool wrapper (didapi's *db.DB satisfies this interface).
//   - job: the Job constructed by Registry.New. The worker writes counters
//     and emits events through this; closes the event channel when done.
//   - entries: parsed input from one of the Parse* helpers.
//   - parseWarnings: warnings the parser surfaced before this worker ran.
//     They're emitted as RowError events at the start of the run so the
//     admin sees them in the same UI as runtime errors.
//
// Side effects:
//
//   - INSERT INTO dids (e164, supplier_id, country_iso, did_type,
//     supplier_channel_cap, status) ... ON CONFLICT (e164) DO NOTHING.
//   - Updates per-row Counters.
//   - Emits Init, then per-row Row events, periodic Progress events, and
//     a final Done event. Closes job.Events after Done.
func Run(ctx context.Context, db DB, job *Job, entries []Entry, parseWarnings []string) {
	started := time.Now()
	job.total.Store(int64(len(entries)))
	job.setStatus(StatusRunning, "")
	job.emit(Event{
		Type:  EventInit,
		Total: len(entries),
	})

	// Replay parse-time warnings into the same event stream the admin is
	// watching, so they don't have to look in two places.
	for _, w := range parseWarnings {
		job.errors.Add(1)
		job.emit(Event{
			Type:      EventRow,
			RowStatus: RowError,
			Reason:    w,
		})
	}

	// Defer the close so panics don't strand the SSE reader.
	defer func() {
		if r := recover(); r != nil {
			job.setStatus(StatusErrored, "panic during import")
			job.emit(Event{
				Type:    EventDone,
				Message: "worker panicked; partial import preserved",
				Total:   int(job.total.Load()),
				Added:   int(job.added.Load()),
				Skipped: int(job.skipped.Load()),
				Errors:  int(job.errors.Load()) + 1,
				Processed:  int(job.processed.Load()),
				DurationMs: time.Since(started).Milliseconds(),
			})
		}
		close(job.Events)
	}()

	// Periodic progress ticker. Even on a fast import (~1000 rows/sec)
	// we want the GUI's progress bar to feel live, hence a 250ms cadence.
	progressTicker := time.NewTicker(250 * time.Millisecond)
	defer progressTicker.Stop()
	emitProgress := func() {
		total, added, skipped, errs, proc := job.Counters()
		job.emit(Event{
			Type:      EventProgress,
			Total:     total,
			Added:     added,
			Skipped:   skipped,
			Errors:    errs,
			Processed: proc,
		})
	}

	for i, e := range entries {
		select {
		case <-ctx.Done():
			job.setStatus(StatusErrored, ctx.Err().Error())
			emitDone(job, started, "import cancelled: "+ctx.Err().Error())
			return
		case <-progressTicker.C:
			emitProgress()
		default:
		}

		// Apply defaults.
		country := e.Country
		if country == "" {
			country = job.Defaults.Country
		}
		if country == "" {
			if iso, ok := MatchCountry(e.E164); ok {
				country = iso
			}
		}
		didType := e.DIDType
		if didType == "" {
			didType = job.Defaults.DIDType
		}
		supID := e.SupplierID
		if supID == 0 {
			supID = job.SupplierID
		}
		cap := e.ChannelCap
		if cap == 0 {
			cap = job.Defaults.ChannelCap
			if cap == 0 {
				cap = -1 // unlimited if neither row nor form specified
			}
		}

		_ = i // reserved for future per-row index in error messages

		// Validate required fields. Country is the most common miss when
		// auto-detection fails (e.g. a 5-digit short code or an unassigned
		// prefix); we report it so the admin can correct the row.
		if country == "" {
			job.errors.Add(1)
			job.processed.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowError,
				Reason:    "country: no prefix match — set Default country on the form",
			})
			continue
		}
		if didType == "" {
			job.errors.Add(1)
			job.processed.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowError,
				Reason:    "did_type missing",
			})
			continue
		}
		if supID == 0 {
			job.errors.Add(1)
			job.processed.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowError,
				Reason:    "supplier_id missing",
			})
			continue
		}

		ct, err := db.Exec(ctx, `
			INSERT INTO dids (e164, supplier_id, country_iso, did_type,
			                  supplier_channel_cap, status)
			VALUES ($1, $2, $3, $4, $5, 'available')
			ON CONFLICT (e164) DO NOTHING`,
			e.E164, supID, country, didType, cap)

		switch {
		case err != nil:
			job.errors.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowError,
				Reason:    classifyDBError(err),
				Country:   country,
				DIDType:   didType,
			})
		case ct.RowsAffected() == 0:
			job.skipped.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowSkipped,
				Reason:    "already exists",
				Country:   country,
				DIDType:   didType,
			})
		default:
			job.added.Add(1)
			job.emit(Event{
				Type:      EventRow,
				E164:      e.E164,
				RowStatus: RowAdded,
				Country:   country,
				DIDType:   didType,
			})
		}
		job.processed.Add(1)
	}

	job.setStatus(StatusDone, "")
	emitProgress() // final counters
	emitDone(job, started, "")
}

func emitDone(job *Job, started time.Time, msg string) {
	total, added, skipped, errs, proc := job.Counters()
	job.emit(Event{
		Type:       EventDone,
		Total:      total,
		Added:      added,
		Skipped:    skipped,
		Errors:     errs,
		Processed:  proc,
		DurationMs: time.Since(started).Milliseconds(),
		Message:    msg,
	})
}

// classifyDBError turns a pgx error into a short human-readable reason for
// the row log. Most common cases we want to recognize:
//
//   - FK violation on country_iso: the auto-detected country isn't in our
//     countries table (e.g. "INT" for international toll-free).
//   - FK violation on supplier_id: stale supplier picked.
//   - check constraint: a country code we don't yet support in the enum.
//
// Anything else falls back to the raw error string.
func classifyDBError(err error) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503": // foreign_key_violation
			switch {
			case strings.Contains(pgErr.ConstraintName, "country"):
				return "country not in our countries table"
			case strings.Contains(pgErr.ConstraintName, "supplier"):
				return "supplier_id not found"
			}
			return "FK violation: " + pgErr.ConstraintName
		case "23505": // unique_violation — should be eaten by ON CONFLICT, but defensive
			return "duplicate (race)"
		case "23514": // check_violation
			return "check constraint: " + pgErr.ConstraintName
		}
	}
	// Generic fallback. Keep it short.
	s := err.Error()
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Sentinel for callers that pass nil entries.
var _ = pgx.ErrNoRows
