// Package billing implements the per-DID anniversary billing run.
//
// At the order's anniversary, we attempt to charge:
//
//	MRC + (channel_count - 2) * channel_monthly_cents
//
// from the user's balance. If insufficient, we step the channel count down by
// 1 and retry, all the way to channel_count = 2 (just MRC). If even MRC fails,
// we suspend the order.
//
// All money moves through the balance_ledger (append-only) inside one
// transaction per order, so any crash leaves a clean state.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"didstorage/internal/domain"
)

type Result struct {
	Processed int
	Charged   int
	Downgrade int
	Suspended int
	Errors    int
}

// Run processes every active order whose next_billing_at is <= now.
// Idempotent on partial failure: each order is its own transaction.
// Orders in kyc_pending or quarantined are skipped — they aren't 'active'.
func Run(ctx context.Context, pg *pgxpool.Pool, log *slog.Logger, now time.Time) (Result, error) {
	var res Result

	rows, err := pg.Query(ctx, `
		SELECT id FROM orders
		 WHERE status = 'active' AND next_billing_at <= $1
		 ORDER BY next_billing_at
	`, now)
	if err != nil {
		return res, fmt.Errorf("select due: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		res.Processed++
		outcome, err := processOne(ctx, pg, log, id, now)
		if err != nil {
			res.Errors++
			log.Error("billing failed", "order_id", id, "err", err)
			continue
		}
		switch outcome {
		case "charged":
			res.Charged++
		case "downgraded":
			res.Downgrade++
		case "cancelled":
			res.Suspended++
		}
	}
	return res, nil
}

// processOne runs one order's billing inside a single transaction.
// Returns one of: "charged" (full count), "downgraded" (lower count), "cancelled".
func processOne(ctx context.Context, pg *pgxpool.Pool, log *slog.Logger, orderID int64, now time.Time) (string, error) {
	tx, err := pg.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		userID              int64
		channelCount        int
		anniversaryDay      int
		nextBillingAt       time.Time
		balanceCents        int64
		mrcCents            int
		channelMonthlyCents int
	)
	err = tx.QueryRow(ctx, `
		SELECT o.user_id, o.channel_count, o.anniversary_day, o.next_billing_at,
		       u.balance_cents, rc.mrc_cents, rc.channel_monthly_cents
		  FROM orders o
		  JOIN users u           ON u.id = o.user_id
		  JOIN rate_cards rc     ON rc.id = o.rate_card_id
		 WHERE o.id = $1 AND o.status = 'active'
		 FOR UPDATE
	`, orderID).Scan(&userID, &channelCount, &anniversaryDay, &nextBillingAt,
		&balanceCents, &mrcCents, &channelMonthlyCents)
	if errors.Is(err, pgx.ErrNoRows) {
		// status changed underneath us — nothing to do
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lock+load: %w", err)
	}

	// Walk down the channel ladder until a charge fits or we hit 0. (0 channels
	// is a valid order state — typically used for paused / testing rentals.)
	target := channelCount
	for target >= 0 {
		// Fee model unchanged: (target-2) extra channels above the MRC base.
		// At target=0 or 1, we charge MRC only (no negative channel fees).
		extra := target - 2
		if extra < 0 {
			extra = 0
		}
		fee := mrcCents + extra*channelMonthlyCents
		if balanceCents >= int64(fee) {
			newBalance := balanceCents - int64(fee)
			mrcPart := mrcCents
			chanPart := fee - mrcCents

			if _, err := tx.Exec(ctx, `
				UPDATE users SET balance_cents = $1 WHERE id = $2
			`, newBalance, userID); err != nil {
				return "", fmt.Errorf("update balance: %w", err)
			}

			// One ledger entry for MRC, one for channel fees (if any).
			if mrcPart > 0 {
				if _, err := tx.Exec(ctx, `
					INSERT INTO balance_ledger (user_id, delta_cents, kind, ref_table, ref_id, balance_after)
					VALUES ($1, $2, 'mrc', 'orders', $3, $4)
				`, userID, -int64(mrcPart), orderID, newBalance+int64(chanPart)); err != nil {
					return "", fmt.Errorf("ledger mrc: %w", err)
				}
			}
			if chanPart > 0 {
				if _, err := tx.Exec(ctx, `
					INSERT INTO balance_ledger (user_id, delta_cents, kind, ref_table, ref_id, balance_after)
					VALUES ($1, $2, 'channel_fee', 'orders', $3, $4)
				`, userID, -int64(chanPart), orderID, newBalance); err != nil {
					return "", fmt.Errorf("ledger channel: %w", err)
				}
			}

			downgraded := target < channelCount
			next := domain.NextAnniversary(now, anniversaryDay)
			if _, err := tx.Exec(ctx, `
				UPDATE orders
				   SET channel_count = $1, next_billing_at = $2
				 WHERE id = $3
			`, target, next, orderID); err != nil {
				return "", fmt.Errorf("update order: %w", err)
			}

			outcome := "charged"
			if downgraded {
				outcome = "downgraded"
			}
			notes := fmt.Sprintf("ok at %d channels", target)
			if downgraded {
				notes = fmt.Sprintf("downgraded from %d to %d (insufficient balance)", channelCount, target)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO billing_runs (order_id, charged_channels, mrc_charged_cents,
				                          channel_charged_cents, outcome, notes)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, orderID, target, mrcPart, chanPart, outcome, notes); err != nil {
				return "", fmt.Errorf("audit: %w", err)
			}

			if err := tx.Commit(ctx); err != nil {
				return "", fmt.Errorf("commit: %w", err)
			}
			log.Info("billing", "order_id", orderID, "outcome", outcome,
				"channels", target, "fee_cents", fee, "user_id", userID)
			return outcome, nil
		}
		target--
	}

	// Couldn't even afford MRC at 0 channels — suspend.
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		   SET status = 'suspended', ended_at = now()
		 WHERE id = $1
	`, orderID); err != nil {
		return "", fmt.Errorf("suspend: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO billing_runs (order_id, charged_channels, mrc_charged_cents,
		                          channel_charged_cents, outcome, notes)
		VALUES ($1, 0, 0, 0, 'cancelled', $2)
	`, orderID, fmt.Sprintf("insufficient balance for MRC %d cents", mrcCents)); err != nil {
		return "", fmt.Errorf("audit suspend: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit suspend: %w", err)
	}
	log.Warn("billing suspended order", "order_id", orderID, "user_id", userID,
		"mrc_cents", mrcCents, "balance_cents", balanceCents)
	return "cancelled", nil
}
